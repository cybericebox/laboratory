# Node Agent — спецификация требований

> Источник: `cybericebox-spec-v1.md` §7 (несущий сетевой слой CNI+OVS, 7-table pipeline), §8 (Device/Connection,
> DHCP-сервер, broadcast-домены), §10 (CRD-контракт чтения).

## Назначение

Node Agent — privileged DaemonSet на каждой ноде кластера лабораторий. Программирует локальный OVS через `libovsdb` (
port/bridge/interface) и OpenFlow 1.3 (`br-ovs.mgmt`), материализует физические сегменты лаб из Connection/Device CRD,
открывает gRPC-Unix-сокет для CNI-плагинов. Реализует 7-табличный OpenFlow-pipeline из §7 (нормализация → разброс →
learning → lookup → диспетчер/flood).

## Карта реализации

| Файл                                      | Роль                                                                          |
|-------------------------------------------|-------------------------------------------------------------------------------|
| `cmd/node-agent/main.go`                  | Entry-point: init OVS+FlowManager+gRPC, регистрация 3 reconcilers             |
| `cmd/node-agent/config.go`                | env-конфиг: NODE_NAME, OVS_SOCK, GRPC_SOCK, BRIDGE=br-ovs, PROC_ROOT          |
| `cmd/node-agent/ovsdb.go`                 | libovsdb-клиент: ensureBridge, AddInternalPort, AddGenevePort, DelPort, mutex |
| `cmd/node-agent/ovsmodel.go`              | model-types для OVSDB-схемы (Open_vSwitch/Bridge/Port/Interface)              |
| `cmd/node-agent/openflow.go`              | FlowManager: AddEgressFlow / AddIngressFlow / AddLocalSwitchFlow / Del*       |
| `cmd/node-agent/ofclient/client.go`       | OF 1.3 клиент: HELLO, FLOW_MOD, PORT_DESC, echo keepalive                     |
| `cmd/node-agent/grpc.go`                  | gRPC-сервер: AddPort, DeletePort, GetPodAnnotation, cache podUID→netns        |
| `cmd/node-agent/netns.go`                 | netns ops: FindPodNetNS, MoveToNetNS, RenameInNetNS, ConfigureInNetNS         |
| `cmd/node-agent/connection_reconciler.go` | Реализует Connection → OVS-порты + flows                                      |
| `cmd/node-agent/lab_reconciler.go`        | Создаёт `lab-<name>` / `gw-<name>` для VPN/gateway pod'ов лабы                |
| `cmd/node-agent/device_reconciler.go`     | Управляет ovs-cleanup finalizer на Device                                     |
| `internal/ovsnames/names.go`              | Стабильные ≤15-char OVS-имена для lab/gw-интерфейсов                          |
| `config/node-agent/daemonset.yaml`        | DaemonSet manifest                                                            |

## Статус-таксономия — см. [operator.md](./operator.md#условные-обозначения-статуса).

---

## Требования

### OVS bridge и base-уровень

#### REQ-NA-001: Единый bridge `br-ovs` на ноде

- **Источник:** §7 «OVS — privileged DaemonSet на каждой ноде»; имплицитно один bridge на ноду — все домены живут в нём.
- **Статус:** ✅ Implemented
- **Реализация:** `ovsdb.go:ensureBridge` (создаёт row Bridge `br-ovs`, добавляет в `Open_vSwitch.bridges`);
  `config.go:Bridge="br-ovs"`.
- **Что считать выполненным:** `ovs-vsctl list-br` показывает ровно один `br-ovs`; повторный запуск node-agent не
  создаёт дубликата.

#### REQ-NA-002: Privileged DaemonSet с OVS-userspace внутри

- **Источник:** §7 «OVS — privileged DaemonSet на каждой ноде (ovs-vswitchd + ovsdb-server)».
- **Статус:** ✅ Implemented
- **Реализация:** `config/node-agent/daemonset.yaml` — container `ovs` (privileged: true, command
  `/node-agent/bin/start-ovs.sh`); container `node-agent` (NET_ADMIN+SYS_PTRACE).
- **Что считать выполненным:** в каждом поде DaemonSet работают ovsdb-server и ovs-vswitchd; их сокеты доступны
  контейнеру node-agent через shared volume `ovs-run`.

#### REQ-NA-003: libovsdb для управления (NB-OVN не используется)

- **Источник:** §7 «динамически программирует OVS через libovsdb (Go): бриджи, порты, OpenFlow, Geneve».
- **Статус:** ✅ Implemented
- **Реализация:** `ovsdb.go` использует `github.com/ovn-org/libovsdb` для bridge/port/interface; OpenFlow — через
  отдельный socket.
- **Что считать выполненным:** все мутации OVSDB идут через `Transact` с моделями из `ovsmodel.go`; нет вызовов
  `exec.Command("ovs-vsctl", ...)`.

#### REQ-NA-004: Идемпотентность OVSDB-операций

- **Источник:** §7 «CNI вызывается на ADD/DEL один раз; инкрементальные изменения...».
- **Статус:** ✅ Implemented
- **Реализация:** `ovsdb.go:AddInternalPort`/`AddGenevePort`/`DelPort` сначала вызывают `findPort`; повторный вызов =
  no-op.
- **Что считать выполненным:** повторные ADD не создают второй порт; повторные DEL не возвращают ошибку.

---

### Разделение ответственности оператор/агент

#### REQ-NA-010: Агент программирует только локальный OVS своей ноды

- **Источник:** §7 «оператор — аллокация и кросс-нодовая картина; нода-агент — локальное программирование OVS».
- **Статус:** ✅ Implemented
- **Реализация:** `connection_reconciler.go:reconcileCreate` пропускает endpoints, чьи
  `Device.Status.NodeName != r.NodeName` (порт создаётся только если устройство на этой ноде; Geneve-порт к remote — но
  это всё ещё локальная программируемая запись).
- **Что считать выполненным:** на ноде A создаются OVS-порты только для устройств, чей `status.nodeName == A`; на ноде
  B — только B-локальных устройств.

#### REQ-NA-011: Запись port-status обратно в Connection.status

- **Источник:** §10 (Connection.status.ports заполняется агентом).
- **Статус:** ✅ Implemented
- **Реализация:** `connection_reconciler.go:reconcileCreate` собирает `desired []ConnectionPortStatus` (Device,
  Interface, PortID, NodeName, NodeAddress, Connected), `r.Status().Update`.
- **Что считать выполненным:** для каждого endpoint появляется запись `connection.status.ports[]` с заполненным `portID`
  и `connected=true`.

---

### Транспорты подключения (§7)

#### REQ-NA-020: Internal-порт для подключения под↔OVS-домен

- **Источник:** §7 «под ↔ OVS-домен — OVS `type=internal`-порт».
- **Статус:** ✅ Implemented
- **Реализация:** `ovsdb.go:AddInternalPort` (type="internal"); `connection_reconciler.go` вызывает её для каждого
  локального device-endpoint, затем `MoveToNetNS(pKey, netnsPath)`.
- **Что считать выполненным:** в OVS существует Port с `Interface.type=internal`; в netns пода видим интерфейс, на
  хосте — нет.

#### REQ-NA-021: Patch-пара для switch↔switch (одна нода)

- **Источник:** §7 «switch ↔ switch — patch-пара, каждый конец — порт в VNI своего свича... Patch — всегда внутри ноды».
- **Статус:** ❌ Missing
- **Реализация:** нет. `OVSManager` не имеет `AddPatchPort`; `connection_reconciler.go` для switch↔switch endpoints не
  делает ничего особенного — оба конца просто помечаются `Connected=true` без OVS-объекта.
- **Что считать выполненным:** при Connection между двумя `unmanaged-switch` устройствами создаётся пара port-ов type=
  `patch` с `options.peer` друг на друга; flow `t0` обрабатывает оба входа.

#### REQ-NA-022: Один Geneve-порт на ноде с `remote_ip=flow, key=flow`

- **Источник:** §7 «Один Geneve-порт на ноде: `options:remote_ip=flow`, `options:key=flow`. Адрес удалённого VTEP и VNI
  проставляются во flow».
- **Статус:** ⚠️ Partial
- **Реализация:** `ovsdb.go:AddGenevePort` создаёт порт с `remote_ip=<конкретный IP>, key=flow` — по одному порту на
  каждую remote-ноду (имя через `genevePortName(remoteAddr)`).
- **Что считать выполненным:** на ноде существует ровно один Geneve-порт `genev_sys`; адрес VTEP проставляется через
  `set_field:tun_dst` во flow (`reg1` или прямой move в t5 диспетчере).
- **Заметки/gap:** текущая модель «порт-на-ноду» работает, но превращает каждого нового remote в новый OVSDB-объект (
  накладные расходы + засорение конфигурации); §7 явно требует одного порта (как в OVN). Это влияет на t6-flood и
  t5-egress.

#### REQ-NA-023: veth-пара для шейпинга — отложено

- **Источник:** §15 «QoS / пропускная способность на линках — переключением... на veth-пару... В v1 поля нет».
- **Статус:** 🔍 Needs verification (вне v1)
- **Реализация:** не реализовано (как и требует §15).
- **Что считать выполненным:** в v1 нигде не используется veth; добавление позже.

---

### 7-table OpenFlow pipeline (§7) — критический раздел

#### REQ-NA-030: Таблица t0 (нормализация: in_port/Geneve → metadata)

- **Источник:** §7 «t0 — нормализация: проставить metadata (Geneve → move tun_id; локальный порт → load VNI), без
  output».
- **Статус:** ❌ Missing
- **Реализация:** нет. Текущий `openflow.go:AddEgressFlow` ставит flow
  `in_port=local → set_field:tun_id=VNI → output:geneve` в **t0**, что объединяет t0+t5 и не использует `metadata`.
- **Что считать выполненным:** для каждого локального порта в t0 есть запись
  `in_port=X → load:VNI → metadata, resubmit(,1)`; для Geneve-порта в t0 —
  `in_port=GENEVE → move:tun_id → metadata, resubmit(,1)`. Output в t0 отсутствует.

#### REQ-NA-031: Таблица t1 (разброс по metadata: учить/не учить)

- **Источник:** §7 «t1 — разброс по metadata: switch-VNI → t2 (learning); дефолт priority=0 → t3/t4 (без learning)».
- **Статус:** ❌ Missing
- **Реализация:** нет.
- **Что считать выполненным:** в t1 priority=100 для каждого switch-VNI → `resubmit(,2)`; default priority=0 →
  `resubmit(,3)`. Hub-домены не попадают в learning (идут сразу в lookup→flood).

#### REQ-NA-032: Таблица t2 (learning, две записи по наличию tun_id)

- **Источник:** §7 «t2 — learning, две записи по наличию `tun_id`: tun_id=0 локальный (load reg0=1, reg1=in_port); иначе
  туннельный (load reg0=2, reg1=tun_src)».
- **Статус:** ❌ Missing
- **Реализация:** нет.
- **Что считать выполненным:** t2 содержит две статичные записи с `learn`-действиями, программирующими t3 (см.
  REQ-NA-033).

#### REQ-NA-033: Таблица t3 (выученные MAC, `idle_timeout=300`)

- **Источник:** §7 «t3 — выученные MAC (наполняет learn, idle_timeout=300, ключ = metadata+eth_dst, значение = load
  reg0,reg1). Дефолт priority=0 оставляет reg0=0».
- **Статус:** ❌ Missing
- **Реализация:** нет (требует поддержки `learn`-action в ofclient).
- **Что считать выполненным:** записи t3 создаются OVS-ом автоматически через learn, истекают через 5 минут idle; дефолт
  priority=0 → `load:0 → reg0`.

#### REQ-NA-034: Таблица t4 (lookup: сброс reg0/reg1 → resubmit t3, t5)

- **Источник:** §7 «t4 — lookup: сброс reg0/reg1, resubmit в t3, затем в t5».
- **Статус:** ❌ Missing
- **Реализация:** нет.
- **Что считать выполненным:** одна статическая запись `load:0->reg0, load:0->reg1, resubmit(,3), resubmit(,5)`.

#### REQ-NA-035: Таблица t5 (диспетчер + egress: 3 записи по reg0)

- **Источник:** §7 «t5 — диспетчер + egress: reg0=0 → flood(t6); reg0=1 → output:reg1; reg0=2 → move metadata→tun_id,
  move reg1→tun_dst, output:GENEVE».
- **Статус:** ❌ Missing
- **Реализация:** нет.
- **Что считать выполненным:** три статические записи; egress через единый Geneve-порт с переменными `tun_id` и
  `tun_dst`.

#### REQ-NA-036: Таблица t6 (flood per VNI: локальные порты + Geneve к удалённым нодам)

- **Источник:** §7 «t6 — flood: по записи на VNI, прямые output (без group). Полный перечень локальных портов домена +
  Geneve ко всем нодам домена».
- **Статус:** ⚠️ Partial (через `OFPG_NORMAL` group, не t6)
- **Реализация:** `openflow.go:AddLocalSwitchFlow` использует OVS reserved group `OFPG_NORMAL` для traditional L2-bridge
  поведения (MAC learning встроен в OVS). Это альтернатива 7-table pipeline, но не она.
- **Что считать выполненным:** в t6 для каждого VNI одна запись с явным списком всех локальных портов + множественных
  move/load+output:GENEVE к каждому удалённому VTEP. Никакой `group=NORMAL`.

#### REQ-NA-037: Регистры `metadata`, `reg0`, `reg1` и Nicira-extensions

- **Источник:** §7 «Регистры: metadata = VNI домена; reg0 = тип результата lookup; reg1 = destination».
- **Статус:** ❌ Missing
- **Реализация:** `ofclient` поддерживает только `OXM_OF_IN_PORT` (8 байт) и `OXM_OF_TUNNEL_ID` (12 байт); нет
  `NXM_NX_METADATA`, `NXM_NX_REG0/REG1`, нет `NXM_NX_TUN_IPV4_SRC/DST`. Actions: только OUTPUT, SET_FIELD(tun_id),
  GROUP. Нет `learn`, `move`, `load (reg)`, `resubmit`.
- **Что считать выполненным:** ofclient уметь сериализовать `learn(...)`, `move:src->dst`, `load:val->reg`,
  `resubmit(,table)` через NXAST/OFPAT_EXPERIMENTER; матчи по `metadata`/`reg`/`tun_src`.

#### REQ-NA-038: Распределение ответственности между статичными и динамичными таблицами

- **Источник:** §7 «t0, t1, t6 — агент (динамически)... t2, t4, t5 — статичны, ставятся один раз при инициализации...
  t3 — наполняет сам OVS (learn)».
- **Статус:** ❌ Missing
- **Реализация:** нет разделения; flow управление сейчас линейно (в t0).
- **Что считать выполненным:** при подключении OF-клиента к bridge ставятся 6 статических записей (t2×2, t4×1, t5×3);
  агент динамически обновляет t0/t1/t6 на каждое подключение порта/изменение домена.

---

### Конкретные потоки данных (поверх pipeline)

#### REQ-NA-050: Cross-node трафик через Geneve с VNI в `tun_id`

- **Источник:** §7 «touvers options:key=flow... VNI проставляется во flow».
- **Статус:** ⚠️ Partial
- **Реализация:** `openflow.go:AddEgressFlow`/`AddIngressFlow` ставит VNI через `set_field:tun_id` — но в t0, а не в t5.
  Работает функционально, но не соответствует архитектуре pipeline.
- **Что считать выполненным:** Geneve-egress всегда через t5; `tun_id=metadata`, `tun_dst=reg1`.

#### REQ-NA-051: Same-node L2-передача с MAC-learning

- **Источник:** §7 (через t2 learn → t3 → t5 reg0=1).
- **Статус:** ⚠️ Partial
- **Реализация:** `AddLocalSwitchFlow` → group=NORMAL (встроенный MAC learn OVS). Работает, но не наша learn-таблица.
- **Что считать выполненным:** локальный unicast не идёт через NORMAL; он идёт через t3-cache, заполненный learn-action.

#### REQ-NA-052: Hub — flood без learning

- **Источник:** §8 «hub — flood без learning»; §7 «hub-домены в t0 уходят сразу в flood, в t1/t2/t3 не попадают».
- **Статус:** ❌ Missing (хотя hub и unmanaged-switch — отдельные DeviceType, поведение одинаково).
- **Реализация:** `device_reconciler.go` обрабатывает оба DeviceType одинаково (просто Ready=true);
  `connection_reconciler.go` не различает их во flow-программировании.
- **Что считать выполненным:** для hub-VNI в t1 priority=100 → `resubmit(,4)` (минуя learning t2-t3); flood работает,
  MAC не учатся.

---

### Кросс-нодовая координация (Geneve)

#### REQ-NA-060: Контролер хранит «для VNI — какие remote VTEP»

- **Источник:** §7 «Control-plane состояние "для VNI — какие удалённые VTEP его несут" (перечень нод сегмента) → агент
  генерит flow».
- **Статус:** ✅ Implemented (де-факто через Device.status.nodeAddress)
- **Реализация:** `connection_reconciler.go:reconcileCreate` итерирует `eps` и для каждого remote endpoint берёт
  `Device.Status.NodeAddress` → `genevePortName(...)` → `AddGenevePort + AddIngressFlow/AddEgressFlow`.
- **Что считать выполненным:** при изменении nodeAddress (миграция пода) flow-ы переставляются; при удалении endpoint
  Geneve-связь снимается.

#### REQ-NA-061: Reconciler учитывает изменения Device.status

- **Источник:** §14 «status.node известен → оператор посчитал Connection.status.nodes → нода-агенты программируют
  локальный OVS».
- **Статус:** ✅ Implemented
- **Реализация:** `connection_reconciler.go:SetupWithManager` имеет
  `Watches(&Device{}, EnqueueRequestsFromMapFunc(connectionsForDevice))` → при обновлении Device все его Connections
  re-reconcile.
- **Что считать выполненным:** при переезде пода между нодами Connection пере-программируется в течение секунд.

---

### Network namespace operations

#### REQ-NA-070: Поиск pod netns по UID через /proc/*/cgroup

- **Источник:** §7 (агент должен моститься в netns пода для перемещения интерфейса).
- **Статус:** ✅ Implemented
- **Реализация:** `netns.go:FindPodNetNS` сканирует `/proc/<pid>/cgroup`, ищет podUID с дефисами или подчёркиваниями (
  нормализация для разных runtimes).
- **Что считать выполненным:** для свежесозданного пода функция возвращает `/proc/<pid>/ns/net` за <1сек.

#### REQ-NA-072: Move + Rename + Configure в netns

- **Источник:** §7 (внутренняя нога пода настраивается агентом).
- **Статус:** ✅ Implemented
- **Реализация:** `netns.go:MoveToNetNS` (LinkSetNsFd); `RenameInNetNS` (внутри netns); `ConfigureInNetNS` (MAC +
  IP/prefix + up).
- **Что считать выполненным:** после AddPort через gRPC внутри пода виден интерфейс с правильным именем, MAC, IP, link
  up.

#### REQ-NA-073: Безопасность переключения netns

- **Источник:** общая корректность Go runtime (один goroutine не должен застрять в чужом netns).
- **Статус:** ✅ Implemented
- **Реализация:** `netns.go:inNetNS` — `runtime.LockOSThread`, восстановление с `panic` при невозможности вернуться в
  исходный netns.
- **Что считать выполненным:** при ошибке `netns.Set(origNS)` процесс падает (не молча продолжает в чужом netns).

---

### gRPC-сервис (для CNI-плагинов)

#### REQ-NA-080: gRPC на host-path Unix-сокете

- **Источник:** §7 (CNI вызывает агент, не K8s API).
- **Статус:** ✅ Implemented
- **Реализация:** `main.go` и `grpc.go:startGRPCServer` — listen `unix:/run/cybericebox/node-agent.sock`; DaemonSet
  монтирует `grpc-dir` как `hostPath: /run/cybericebox`.
- **Что считать выполненным:** CNI-плагин на хосте может соединиться с `/run/cybericebox/node-agent.sock`; путь доступен
  любому процессу в `root` namespace.

#### REQ-NA-081: RPC AddPort / DeletePort

- **Источник:** §7 (агент создаёт OVS-порт по запросу CNI).
- **Статус:** ✅ Implemented
- **Реализация:** `grpc.go:AddPort` (создаёт internal port → move в netns → переименование); `DeletePort` (удаляет все
  порты для podUID).
- **Что считать выполненным:** CNI ADD приводит к появлению нового OVS-порта в `br-ovs` и интерфейса внутри пода; CNI
  DEL — удалению.

#### REQ-NA-082: RPC GetPodAnnotation (вместо доступа CNI к K8s API)

- **Источник:** §7 (CNI не должен ходить в K8s API); общее архитектурное решение проекта.
- **Статус:** ✅ Implemented
- **Реализация:** `grpc.go:GetPodAnnotation` — читает Pod через `mgr.GetClient()`, возвращает значение по ключу + флаг
  Found.
- **Что считать выполненным:** запрос с `(namespace, name, key)` возвращает `(value, true)` или `("", false)`;
  NotFound — не ошибка.

---

### Reconcilers

#### REQ-NA-090: ConnectionReconciler — материализация портов и flows

- **Источник:** §10 (Connection — единица топологии); §7 (агент программирует OVS).
- **Статус:** ⚠️ Partial
- **Реализация:** `connection_reconciler.go` создаёт internal port, move в netns, конфигурит IP/MAC, программирует
  Geneve+flows; cleanup на DEL.
- **Что считать выполненным:** все типы Connection (device↔device, switch↔device, switch↔switch) обрабатываются.
- **Заметки/gap:** switch↔switch не использует patch (REQ-NA-021); switch↔device без явного VNI берёт VNI из
  switch.Status.VNI — OK; но flow programming для switch-связей использует t0 а не 7-table pipeline.

#### REQ-NA-091: LabIfaceReconciler — `lab-<name>` / `gw-<name>` для VPN/gateway pod'ов

- **Источник:** §9 (VPN/gateway — синглтоны лабы с одним портом в OVS-домен).
- **Статус:** ✅ Implemented
- **Реализация:** `lab_reconciler.go:Reconcile` — для каждой Lab с `Spec.VPN.Enabled` и `Status.VPN.CIDR != ""` находит
  pod `app=vpn` на этой ноде, создаёт internal port `lab-<labName>` (или хеш), перемещает в netns. Аналогично
  `app=gateway` → `gw-<labName>`.
- **Что считать выполненным:** в pod'е VPN видна нога `lab-<n>`, в pod'е gateway — `gw-<n>`.

#### REQ-NA-092: DevicePortReconciler — только finalizer

- **Источник:** §10 (FinalizerOVSCleanup на Device).
- **Статус:** ✅ Implemented
- **Реализация:** `device_reconciler.go` — добавляет finalizer на switch/hub (всегда) и на container/vm с
  `NodeName=NodeName`; при удалении ждёт, пока все Connection-finalizer-ы сняты, потом убирает finalizer Device.
- **Что считать выполненным:** Pod удаляется только после OVS-cleanup.

---

### DHCP-сервер (§8)

#### REQ-NA-100: Реальный DHCP-сервер на broadcast-домене

- **Источник:** §8 «Реальный DHCP-сервер (не эмуляция): сервер на шлюзе broadcast'ит по всему broadcast-домену».
- **Статус:** ❌ Missing
- **Реализация:** в node-agent — нет. В `cmd/gateway/dhcp.go` есть DHCP-сервер, который работает в pod'е gateway — это
  «сервер на шлюзе», как и сказано в §8.
- **Что считать выполненным:** **в node-agent DHCP не нужен** — сервер живёт в pod'е internet-gateway и/или
  vpn-singleton. См. [gateway.md](./gateway.md). В node-agent остаётся только подключение интерфейса pod'а к нужному
  VNI.
- **Заметки/gap:** требование вынесено в gateway. Здесь оставлено как явное «не реализуем».

---

### DaemonSet manifest

#### REQ-NA-110: HostPaths для CNI-bin и conflist

- **Источник:** §7 (CNI-binary живёт в `/opt/cni/bin`, conflist в `/etc/cni/net.d`).
- **Статус:** ✅ Implemented
- **Реализация:** `daemonset.yaml` initContainer копирует `cni-gate` в `/opt/cni/bin` и `10-cybericebox.conflist` в
  `/etc/cni/net.d`; volumes — hostPath с `DirectoryOrCreate`.
- **Что считать выполненным:** после запуска DaemonSet на ноде существуют `/opt/cni/bin/cni-gate` и
  `/etc/cni/net.d/10-cybericebox.conflist`.

#### REQ-NA-111: Privileged для OVS, capabilities для node-agent

- **Источник:** §7 (OVS — privileged).
- **Статус:** ✅ Implemented
- **Реализация:** `daemonset.yaml` — `ovs` container: `securityContext.privileged: true`; `node-agent`:
  `capabilities.add=[NET_ADMIN, SYS_PTRACE]`.
- **Что считать выполненным:** ovsdb-server и vswitchd работают без отказов; node-agent может писать в netns других
  подов.

#### REQ-NA-112: Mount host /proc и /run/netns

- **Источник:** §7 (агент должен видеть netns других подов).
- **Статус:** ✅ Implemented
- **Реализация:** volumes `host-proc: /proc` (ro), `host-netns: /run/netns` (ro); mounts в node-agent.
- **Что считать выполненным:** `FindPodNetNS("/host/proc", podUID)` работает; `MoveToNetNS` через `/run/netns/<id>`
  тоже.

#### REQ-NA-113: Один gRPC-socket per-node на hostPath

- **Источник:** §7 (CNI ходит к локальному агенту; socket доступен с хоста).
- **Статус:** ✅ Implemented
- **Реализация:** volume `grpc-dir` — `hostPath: /run/cybericebox`, mount в node-agent контейнере; sockaddr
  `unix:/run/cybericebox/node-agent.sock`.
- **Что считать выполненным:** на ноде есть `/run/cybericebox/node-agent.sock`, доступ от root.

---

### ofclient (OpenFlow 1.3 wire protocol)

#### REQ-NA-120: OF 1.3 HELLO + FLOW_MOD + PORT_DESC + ECHO

- **Источник:** §7 (управление OVS через OpenFlow).
- **Статус:** ✅ Implemented
- **Реализация:** `ofclient/client.go` — HELLO sync handshake, async readLoop с ECHO_REPLY, channel-based PORT_DESC
  dispatch, FLOW_MOD ADD/DELETE.
- **Что считать выполненным:** клиент держит соединение неограниченно долго; OVS не закрывает по таймауту.

#### REQ-NA-121: Поддержка Nicira-extensions (learn, move, load, resubmit, NXM_NX_*)

- **Источник:** §7 (7-table pipeline требует Nicira actions).
- **Статус:** ❌ Missing
- **Реализация:** есть только OFPAT_OUTPUT, OFPAT_SET_FIELD(tun_id), OFPAT_GROUP; OXM — только IN_PORT и TUNNEL_ID.
- **Что считать выполненным:** ofclient сериализует `NXAST_LEARN`, `NXAST_RESUBMIT`, `NXAST_REG_MOVE`, `NXAST_REG_LOAD`;
  матчи по `NXM_NX_METADATA` (OXM_OF_METADATA — тоже OK), `NXM_NX_REG0`, `NXM_NX_REG1`, `NXM_NX_TUN_IPV4_SRC`,
  `NXM_NX_TUN_IPV4_DST`.

---

### Совместимость с типами устройств (§8)

#### REQ-NA-130: container — обычный pod

- **Источник:** §8 «container — обычный docker-образ».
- **Статус:** ✅ Implemented
- **Реализация:** Pod создаётся оператором; node-agent привязывает порты как для любого pod'а.
- **Что считать выполненным:** контейнер с лабовыми портами стартует и получает IP по static/dhcp.

#### REQ-NA-131: vm — KubeVirt-перспектива

- **Источник:** §8 «vm (перспектива) — KubeVirt».
- **Статус:** ❌ Missing
- **Реализация:** нет интеграции с KubeVirt; type=vm обрабатывается как container.
- **Что считать выполненным:** для type=vm создаётся `kubevirt.io/VirtualMachine`; OVS-порт подключается к ВМ.

#### REQ-NA-132: unmanaged-switch / hub — без Pod, только OVS-домен

- **Источник:** §8 (это «логические» устройства, не контейнеры).
- **Статус:** ⚠️ Partial
- **Реализация:** `device_reconciler.go` сразу проставляет Ready=true; VNI аллоцируется оператором; switch-endpoints в
  Connection помечаются `Connected=true` без OVS-порта.
- **Что считать выполненным:** для switch — VNI существует, hub-VNI обходит learning (REQ-NA-052); для патч-связей
  создаются patch-порты (REQ-NA-021).
- **Заметки/gap:** различия switch vs hub не отражены в flow programming.

---

## Outstanding gaps

### Блокеры (без них §7 не выполнен)

- ❌ **REQ-NA-030..038** — 7-table OpenFlow pipeline целиком: нормализация, learning, lookup, диспетчер. Текущий код
  использует только t0 и встроенный `group=NORMAL`. Без этого нет поведения, описанного в §7.
- ❌ **REQ-NA-121** — Nicira-extensions в ofclient. Без них pipeline даже невозможно запрограммировать.
- ❌ **REQ-NA-021** — patch-пары для switch↔switch. Без них цепочки коммутаторов не работают.
- ⚠️ **REQ-NA-022** — один Geneve-порт с `remote_ip=flow`. Сейчас по одному на каждую remote-ноду.

### Важно

- ❌ **REQ-NA-052** — hub vs switch различение во flow.
- ⚠️ **REQ-NA-132** — типы switch/hub во flow programming.
- ❌ **REQ-NA-131** — VM (KubeVirt).
- 🔍 **REQ-NA-023** — veth/QoS (отложено).

### Желательно

- ⚠️ **REQ-NA-050** — Geneve-egress через t5, не t0.
- ⚠️ **REQ-NA-051** — same-node forwarding через t3-cache, не group=NORMAL.

---

## Кросс-ссылки

- CNI-плагины, использующие gRPC node-agent: [cni-plugins.md](./cni-plugins.md)
- LabGroup → namespace, контракт CRD: [operator.md](./operator.md)
- VPN-серверу нужна нога `lab-<name>` от node-agent: [vpn.md](./vpn.md)
- Internet-gateway `gw-<name>` + DHCP: [gateway.md](./gateway.md)
