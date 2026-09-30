# Operator — спецификация требований

> Источник: `cybericebox-spec-v1.md` §1 (термины), §2 (адресация), §10 (CRD-модель), §11 (Pool-аллокатор), §13 (границы
> изоляции), §14 (поток разворачивания).

## Назначение

Operator — центральный controller-runtime манагер, реконсайлящий CRD-граф (LabGroup → Lab → Device/Connection, плюс
LabGroupClient и Pool). Аллоцирует VNI/IP/lab-подсети, материализует дочерние ресурсы, создаёт Kubernetes-объекты (
Namespace, Deployment, Service, NetworkPolicy, Secret). Не программирует OVS и не управляет данными WireGuard напрямую —
это делают node-agent и VPN-сервер по фактам из status.

## Карта реализации

| Файл                                                          | Роль                                                                           |
|---------------------------------------------------------------|--------------------------------------------------------------------------------|
| `cmd/main.go`                                                 | Entry-point; регистрация всех reconcilers, leader election, метрики/webhook    |
| `internal/controller/laboratory/labgroup_controller.go`       | LabGroup reconciler                                                            |
| `internal/controller/laboratory/lab_controller.go`            | Lab reconciler (VNI/IP, дочерние Device/Connection, web Service+NetworkPolicy) |
| `internal/controller/laboratory/device_controller.go`         | Device reconciler (Pod-материализация)                                         |
| `internal/controller/laboratory/connection_controller.go`     | Connection reconciler (агрегация port-status)                                  |
| `internal/controller/laboratory/labgroupclient_controller.go` | LabGroupClient reconciler (IP, Secret, keypair)                                |
| `pkg/api/pool/pool.go`                                        | Allocator: bitmap, multi-pool overflow, label-state                            |
| `api/laboratory/v1alpha1/*.go`                                | CRD-типы (LabGroup, Lab, Device, Connection, LabGroupClient)                   |
| `api/allocation/v1alpha1/pool_types.go`                       | CRD-тип Pool                                                                   |

## Условные обозначения статуса

- ✅ **Implemented** — код есть, выполняет требование целиком.
- ⚠️ **Partial** — есть частично; gap описан в «Заметки/gap».
- ❌ **Missing** — кода нет.
- 🔍 **Needs verification** — код выглядит на месте, нужна живая проверка (e2e/нагрузка).

---

## Требования

### CRD-иерархия и scope

#### REQ-OP-001: LabGroup — cluster-scoped CRD

- **Источник:** §10 «`LabGroup` — cluster-scoped».
- **Статус:** ✅ Implemented
- **Реализация:** `api/laboratory/v1alpha1/labgroup_types.go` (kubebuilder marker `+kubebuilder:resource:scope=Cluster`,
  `+genclient:nonNamespaced`).
- **Что считать выполненным:** kubectl get labgroup без `-n` возвращает объект; CRD `kind: LabGroup` имеет
  `spec.scope: Cluster`.

#### REQ-OP-002: Lab — namespaced CRD внутри namespace группы

- **Источник:** §10 «`Lab` — namespaced (в namespace группы); owner = `LabGroup`».
- **Статус:** ⚠️ Partial
- **Реализация:** `api/laboratory/v1alpha1/lab_types.go` (namespaced по умолчанию).
- **Что считать выполненным:** Lab создаётся в namespace `labgroup-<UID>`, имеет OwnerReference на LabGroup, удаление
  LabGroup каскадно удаляет Lab.
- **Заметки/gap:** owner-ref на LabGroup не выставляется автоматически (cross-namespace owner refs запрещены K8s между
  cluster-scoped owner и namespaced child, но cluster-scoped → namespaced OK). LabGroup reconciler сейчас только создаёт
  namespace; обратная привязка Lab→LabGroup не enforced.

#### REQ-OP-003: Device/Connection — controller-owned, read-only для пользователя

- **Источник:** §10 «`Device` / `Connection` — namespaced дочерние, controller-owned, read-only для пользователя».
- **Статус:** ⚠️ Partial
- **Реализация:** `internal/controller/laboratory/lab_controller.go:materializeDevices`, `materializeConnections` (
  выставляет OwnerReference на Lab).
- **Что считать выполненным:** прямое создание Device/Connection пользователем должно отвергаться валидатором или быть
  бессмысленным (объект пересоздаётся reconciler-ом).
- **Заметки/gap:** нет admission webhook, который запрещает пользователю напрямую создавать Device/Connection. Read-only
  enforcement отсутствует.

#### REQ-OP-004: Namespace per LabGroup

- **Источник:** §10 «контроллер создаёт под неё отдельный namespace `labgroup-<id>`».
- **Статус:** ✅ Implemented
- **Реализация:** `labgroup_controller.go:ensureNamespace` (формат имени `labgroup-<UID>`, label
  `laboratory.cybericebox.com/group`).
- **Что считать выполненным:** при создании LabGroup появляется namespace со стабильным именем; при удалении LabGroup
  namespace удаляется (finalizer ждёт удаления).

---

### LabGroup reconciler

#### REQ-OP-010: Генерация или принятие WireGuard-keypair

- **Источник:** §10 «при переданных ключах контроллер проверяет: pub соответствует priv, и pub не занят другим
  сервером».
- **Статус:** ⚠️ Partial
- **Реализация:** `labgroup_controller.go:ensureVPNKeypair` (генерит, если `Spec.VPN.KeypairSecretRef == nil`; принимает
  Secret иначе).
- **Что считать выполненным:** при переданном Secret выполняются проверки соответствия pub↔priv и глобальной
  уникальности pub.
- **Заметки/gap:** нет проверки соответствия priv↔pub в переданном Secret; нет проверки глобальной уникальности pub (
  REQ-OP-011 ниже).

#### REQ-OP-011: Глобальная уникальность LabGroup.status.vpn.publicKey

- **Источник:** §10 «`vpn.publicKey` глобально уникален по кластеру — это routing-key демукса; дубль → две группы
  неразличимы».
- **Статус:** ❌ Missing
- **Реализация:** нет
- **Что считать выполненным:** при попытке создать LabGroup с тем же pubkey что у уже существующей — отказ создания (
  validation webhook или явная проверка в reconciler) и Phase=Failed.
- **Заметки/gap:** инвариант критический для маршрутизации демукса; сейчас не enforced ни в spec validation, ни в
  reconciler.

#### REQ-OP-012: VPN-сервер развёрнут как Deployment×1

- **Источник:** §4 «Один на группу», §14 «VPN-сервер (ключи в Secret) + регистрация».
- **Статус:** ✅ Implemented
- **Реализация:** `labgroup_controller.go:ensureVPNDeployment` (Deployment `vpn` в namespace группы, replicas=1, образ
  `cybericebox/vpn:latest`, envFrom Secret `vpn-server-keypair`).
- **Что считать выполненным:** в `labgroup-<UID>` присутствует Deployment `vpn` с одной репликой; контейнер получает
  приватный ключ через env.

#### REQ-OP-013: Регистрация pubkey→backend в демукс

- **Источник:** §4 «Источник правды `server-pubkey → backend(podIP:port)` — в CRD/реестре», §10
  `status.vpn.backend: <podIP:port>` + `registered: true|false`.
- **Статус:** ❌ Missing
- **Реализация:** нет. `LabGroup.Status.VPN` не содержит полей `Backend`, `Registered`; reconciler не наблюдает за Pod
  VPN-сервера, не пишет podIP в status; демукс читает только pubkey, но не имеет адреса для форварда.
- **Что считать выполненным:** после Ready-фазы Pod-а VPN, `LabGroup.status.vpn.backend == "<podIP>:51820"`,
  `registered == true`; демукс получает событие через watch и обновляет in-memory таблицу.

#### REQ-OP-014: Endpoint в LabGroup.status.vpn.endpoint

- **Источник:** §10 `status.vpn.endpoint: <публичный вход>`.
- **Статус:** ❌ Missing
- **Реализация:** нет. Поле есть в типе, но reconciler его не заполняет.
- **Что считать выполненным:** Endpoint берётся из конфига платформы (env/ConfigMap «публичный вход») и записывается в
  status.

#### REQ-OP-015: Создание pool «vpn-clients» (per-group, IP)

- **Источник:** §11 «IP — пул per-group (`kind: ip`, `groupRef`)», §12 «IP назначает система из per-group IP-пула».
- **Статус:** ✅ Implemented
- **Реализация:** `labgroup_controller.go:ensurePool(..., "vpn-clients", PoolTypeVPNClients, 0, 254)` — offset=0,
  size=254.
- **Что считать выполненным:** в namespace группы существует Pool `vpn-clients-0`, лейбл
  `pool.cybericebox.com/type=vpn-clients`, бит 0 зарезервирован (network address).

#### REQ-OP-016: Создание pool «lab-subnets» (per-group, /24 индексы)

- **Источник:** §2 «N берётся из per-group пула /24-индексов».
- **Статус:** ✅ Implemented
- **Реализация:** `labgroup_controller.go:ensurePool(..., "lab-subnets", PoolTypeLabSubnets, 1, 254)` — offset=1,
  size=254.
- **Что считать выполненным:** Pool `lab-subnets-0` существует; индексы выдаются начиная с N=1 (N=0 не используется,
  т.к. это сетевой адрес `/24`).

#### REQ-OP-017: Finalizer + удаление namespace при удалении LabGroup

- **Источник:** §10 (граница арендатора, тривиальная очистка).
- **Статус:** ✅ Implemented
- **Реализация:** `labgroup_controller.go:reconcileDelete` (finalizer `cybericebox.com/labgroup`, requeue до
  подтверждения NotFound).
- **Что считать выполненным:** удаление LabGroup → namespace удаляется (каскадом VPN, Secret, Pools, всё
  LabGroupClient/Lab внутри).

---

### Lab reconciler

#### REQ-OP-020: Lab.spec — единый источник правды

- **Источник:** §10 «`spec` — полное желаемое состояние, единственный источник правды по топологии, иммутабельно после
  Ready».
- **Статус:** ⚠️ Partial
- **Реализация:** `api/laboratory/v1alpha1/lab_types.go` (`LabSpec{VPN, Internet, Devices, Connections}`); reconciler
  читает только spec.
- **Что считать выполненным:** spec покрывает все 4 секции (vpn, internet, devices, connections) И есть admission
  webhook, запрещающий мутации spec после `phase=Ready`.
- **Заметки/gap:** immutability after Ready не enforced; webhook отсутствует. Пользователь может изменить spec в любой
  момент, поведение reconciler-а на изменение топологии не специфицировано.

#### REQ-OP-021: Per-lab подсети `10.{vpn}.N.0/24` и `10.{inet}.N.0/24`, общий N

- **Источник:** §2 «VPN-диапазон: `10.{vpn}.N.0/24`, internet-диапазон: `10.{inet}.N.0/24`», «структура одинакова во
  всех лабах (меняется только октет N), подсеть детерминированно выводится из N».
- **Статус:** ✅ Implemented
- **Реализация:** `lab_controller.go:ensureSubnetAllocation` (один индекс N из pool `lab-subnets`, `10.8.N.0/24` для
  VPN, `10.9.N.0/24` для Internet; константы `vpnSubnetOctet2=8`, `inetSubnetOctet2=9`).
- **Что считать выполненным:** для каждой Lab `status.vpn.cidr` и `status.internet.cidr` имеют один и тот же N в третьем
  октете; разные лабы получают разные N; при удалении лабы N освобождается.

#### REQ-OP-022: VNI — глобальный пул (один на кластер)

- **Источник:** §11 «VNI — глобальный пул (cluster-scoped, один индекс на кластер)».
- **Статус:** ⚠️ Partial
- **Реализация:** `lab_controller.go` — константы `vniPoolNS="lab-system"`, `vniPoolPrefix="vni"`, `vniPoolSize=65000`;
  Pool создаётся в namespace `lab-system`.
- **Что считать выполненным:** все Lab-и кластера получают VNI из одного логического пула; коллизий между группами нет.
- **Заметки/gap:** Pool — namespaced CRD, не cluster-scoped. Де-факто работает потому что namespace `lab-system`
  одиночный, но семантически не «cluster-scoped». Нет автоматического создания этого namespace и стартового Pool-а при
  бутстрапе оператора — кто создаёт `vni-0` в `lab-system`?

#### REQ-OP-023: Аллокация VNI для switch/hub устройств

- **Источник:** §8 «`unmanaged-switch` / `hub` (Device) | свой VNI — выдаётся при создании устройства».
- **Статус:** ✅ Implemented
- **Реализация:** `lab_controller.go:materializeDevices` (если `tmpl.Type` == unmanaged-switch/hub →
  `vniAllocator.AllocateIndex` → пишется в `device.Status.VNI`); освобождение в `reconcileDelete`.
- **Что считать выполненным:** только switch/hub имеют `status.vni`; container/vm — `vni == nil`.

#### REQ-OP-024: Аллокация VNI для direct-connection

- **Источник:** §8 «Link: устройство ↔ устройство | свой VNI — новый домен из 2 портов»; «Link: switch/hub ↔
  устройство | пустой» (нет VNI); «Link: switch ↔ switch | пустой».
- **Статус:** ✅ Implemented
- **Реализация:** `lab_controller.go:materializeConnections` (флаг `isDirect`: все endpoints — НЕ switch/hub → VNI
  аллоцируется; иначе nil).
- **Что считать выполненным:** для каждой Connection: VNI != nil ⇔ ни один endpoint не switch/hub; для switch-связей VNI
  наследуется из switch.

#### REQ-OP-025: Валидация графа — циклы коммутаторов запрещены

- **Источник:** §8 «Циклы коммутаторов запрещены (STP нет → L2-шторм) — валидация графа».
- **Статус:** ✅ Implemented
- **Реализация:** `lab_controller.go:validateGraph` (DFS по smежности switch-switch через общие Connection; цикл →
  `phase=Failed`).
- **Что считать выполненным:** Lab с двумя switch'ами, соединёнными через два параллельных Connection или замкнутыми в
  треугольник, переходит в `phase=Failed` с reason `SwitchCycleDetected`.

#### REQ-OP-026: Валидация broadcast-доменов (VPN и internet не в одном)

- **Источник:** §8 «VPN и internet в одном broadcast-домене — запрещено».
- **Статус:** ❌ Missing
- **Реализация:** нет.
- **Что считать выполненным:** Lab, где синглтоны vpn и internet оказываются в одной связной компоненте через
  switch/hub, отвергается (или предупреждение в status).

#### REQ-OP-027: Валидация ≤1 DHCP-сервера на broadcast-домен

- **Источник:** §8 «≤1 DHCP-сервер на broadcast-домен (иначе гонка) — проверка по связной L2-компоненте».
- **Статус:** ❌ Missing
- **Реализация:** нет; reconciler не проверяет DHCP-конфликты.
- **Что считать выполненным:** Lab с двумя DHCP-серверами в одной L2-связной компоненте отвергается.

#### REQ-OP-028: Web-exposure — Service + NetworkPolicy

- **Источник:** §5 «вторая нога в кластерной сети + ClusterIP Service», «NetworkPolicy: входящий — только от прокси;
  egress — deny».
- **Статус:** ✅ Implemented
- **Реализация:** `lab_controller.go:ensureWebServices` (ClusterIP Service на `app=<device>`, NetworkPolicy с
  `ingress.from.namespaceSelector=proxy-system && podSelector=app:proxy`, `egress: []`).
- **Имя Service и хост:** `<device>-<labid>` (`names.WebHostLabel`, `labid` = 25 символов base36 от UID объекта Lab,
  с ведущими нулями). Это же первый лейбл публичного хоста `https://<device>-<labid>.<BASE_DOMAIN>`. Два Lab одной группы с
  устройством `web` больше не делят Service и хост. У Service есть лейблы `laboratory.cybericebox.com/lab` и
  `.../device` и `.../lab-id`, по ним прокси относит запрос к лаборатории (см. proxy.md, учёт обращений).
- **Что считать выполненным:** для каждого Device с `exposure.web != nil` существует Service и NetworkPolicy; пакеты не
  от prox-pod-а отбрасываются; egress полностью запрещён.
- **Заметки/gap:** spec §5 говорит о «второй ноге» (отдельный интерфейс) на под — здесь web-нога совпадает с дефолтным
  eth0 (потому что Service использует под селектор). Это OK для текущей реализации, но в спецификации описана
  дополнительная нога. Уточнить у пользователя.

#### REQ-OP-029: Lab.status.access — публичные URL после Ready

- **Источник:** §10 «`access: { ... }` — собирается на Ready (напр. challenge-URL для web)»; §14 «платформа подставляет
  реальные адреса в описание задания».
- **Статус:** ❌ Missing
- **Реализация:** поле `LabStatus.Access` есть в типе, но reconciler его не наполняет.
- **Что считать выполненным:** после `phase=Ready`, для каждого web-exposed device в `lab.status.access` появляется
  `{device, port, protocol, url: "https://<device>.challenges.домен"}`.

#### REQ-OP-030: Finalizers VPN и Gateway при `enabled`

- **Источник:** §10 (наполняется операторами VPN/Gateway).
- **Статус:** ✅ Implemented
- **Реализация:** `lab_controller.go:ensureNetworkFinalizers` (если `spec.vpn.enabled=true` → finalizer
  `cybericebox.com/vpn`; если `spec.internet.enabled=true` → `cybericebox.com/gateway`).
- **Что считать выполненным:** до удаления Lab контроллеры VPN и Gateway успевают снять свои порты/правила и убрать
  finalizer-ы.

---

### Device reconciler

#### REQ-OP-040: Material Pod для container/vm

- **Источник:** §8 «`container` — обычный docker-образ; `vm` (перспектива) — KubeVirt».
- **Статус:** ⚠️ Partial
- **Реализация:** `device_controller.go:createPod` создаёт Pod с одним контейнером (`image: device.Spec.Image`, label
  `app=<device.Spec.Name>`, owner=Device).
- **Что считать выполненным:** для type=container — Pod создаётся и стартует; для type=vm — создаётся
  `kubevirt.io/VirtualMachine` или эквивалент.
- **Заметки/gap:** type=vm обрабатывается как container (создаётся обычный Pod), без интеграции с KubeVirt. Spec
  помечает VM как «перспектива» — допустимо отложить.

#### REQ-OP-041: switch/hub не имеют Pod

- **Источник:** §7 (switch/hub — это объекты OVS, не контейнеры).
- **Статус:** ✅ Implemented
- **Реализация:** `device_controller.go:Reconcile` — switch/hub сразу получают `Ready=true` без создания Pod.
- **Что считать выполненным:** в namespace нет Pod-а с именем switch/hub устройства; `device.status.ready == true`.

#### REQ-OP-042: Заполнение Device.status (NodeName, PodIP, Ready)

- **Источник:** §10 `Device.status` (нужно node-agent для аллокации Geneve-VTEP).
- **Статус:** ⚠️ Partial
- **Реализация:** `device_controller.go:reconcilePod` — Pod.spec.nodeName → status.nodeName; Pod.status.podIP →
  status.podIP; Pod.status.phase==Running → status.ready.
- **Что считать выполненным:** все поля заполнены, плюс `status.nodeAddress` (IP ноды для Geneve VTEP).
- **Заметки/gap:** поле `nodeAddress` существует в типе, но **не заполняется** reconciler-ом. Node-agent должен сам
  вычислять адрес VTEP — но тогда поле в Device.status избыточно.

#### REQ-OP-043: OVS-cleanup finalizer на Device

- **Источник:** §14 (node-agent программирует OVS на удаление).
- **Статус:** ✅ Implemented
- **Реализация:** `lab_controller.go:materializeDevices` ставит `FinalizerOVSCleanup`; `device_controller.go:Reconcile`
  ждёт его снятия node-agent-ом перед завершением.
- **Что считать выполненным:** при удалении Device pod не удаляется немедленно — сначала node-agent снимает OVS-порты и
  убирает finalizer.

---

### Connection reconciler

#### REQ-OP-050: Endpoints — ровно 2 конца

- **Источник:** §8 «Единственный вид — `direct` (ровно 2 конца)».
- **Статус:** ⚠️ Partial
- **Реализация:** `api/laboratory/v1alpha1/connection_types.go` — `+kubebuilder:validation:MinItems=2`.
- **Что считать выполненным:** валидация отвергает Connection с < 2 ИЛИ > 2 endpoints.
- **Заметки/gap:** сейчас `MinItems=2`, нет `MaxItems=2`. Технически можно создать Connection с 3+ endpoints —
  спецификация запрещает.

#### REQ-OP-051: Агрегация port-status в Connection.status.ready

- **Источник:** §10 (node-agent пишет порты, reconciler агрегирует).
- **Статус:** ✅ Implemented
- **Реализация:** `connection_controller.go:Reconcile` (
  `Ready = (len(Ports) == len(Endpoints)) && all(Ports.Connected)`).
- **Что считать выполненным:** Connection переходит в Ready только когда все ноды отчитались о подключённых портах.

#### REQ-OP-052: OVS-cleanup finalizer на Connection

- **Источник:** §14.
- **Статус:** ✅ Implemented
- **Реализация:** `materializeConnections` ставит `FinalizerOVSCleanup`; `connection_controller.go` ждёт его снятия (
  requeue 2с).
- **Что считать выполненным:** Connection не удаляется пока node-agent держит OVS-порты.

---

### LabGroupClient reconciler

#### REQ-OP-060: Pubkey приходит снаружи или генерится система

- **Источник:** §12 «клиент генерит пару, шлёт только pubkey; система приватника не видит» / «запасной: система
  генерит».
- **Статус:** ✅ Implemented
- **Реализация:** `labgroupclient_controller.go:reconcileCreate` (если `Spec.PublicKey == ""` →
  `generateWireGuardKeypair` → privKey пишется в Secret).
- **Что считать выполненным:** при пустом Spec.PublicKey в Secret `client-<name>.data.privateKey` содержит
  сгенерированный приватник; при заданном — только publicKey/assignedIP.

#### REQ-OP-061: IP назначается из per-group pool

- **Источник:** §12 «IP назначает система из per-group IP-пула».
- **Статус:** ✅ Implemented
- **Реализация:** `labgroupclient_controller.go:reconcileCreate` — `NewAllocator("vpn-clients", lgc.Namespace, 254)` →
  `assignedIP = "10.8.0.<idx>/32"`.
- **Что считать выполненным:** `status.assignedIP` имеет вид `10.8.0.X/32`; разные клиенты получают разные X; при
  удалении X освобождается.

#### REQ-OP-062: Уникальность публичного ключа в пределах группы

- **Источник:** §12 «Проверяется только уникальность публичного ключа в пределах WG-сервера группы».
- **Статус:** ❌ Missing
- **Реализация:** нет проверки дубля pubkey.
- **Что считать выполненным:** создание LabGroupClient с уже занятым в группе pubkey отвергается (или второй клиент
  остаётся в Phase=Failed).

#### REQ-OP-063: Полный Secret клиента

- **Источник:** §12 «`serverPublicKey`, `endpoint`, `allowedIPs`, `assignedIP`, `dns?` — не секреты, но отдаются одним
  объектом; `privateKey` — только если система генерила; готовая строка-конфиг генерируется только когда приватник
  доступен».
- **Статус:** ⚠️ Partial
- **Реализация:** `labgroupclient_controller.go:ensureClientSecret` пишет только `publicKey`, `assignedIP`, опц.
  `privateKey`.
- **Что считать выполненным:** в Secret присутствуют все поля из спецификации: `serverPublicKey`, `endpoint`,
  `allowedIPs`, `assignedIP`, опц. `dns`, опц. `privateKey`, опц. готовая строка `wg.conf`.
- **Заметки/gap:** нет `serverPublicKey`, нет `endpoint`, нет `allowedIPs`, нет готовой строки-конфига. Клиент не сможет
  собрать `wg.conf` только из этого Secret.

#### REQ-OP-064: Раздельные зоны секретов

- **Источник:** §12 «`LabGroupClient` — про сетевой доступ; идентичность участника — на платформе; секреты доступа и
  идентичности раздельны».
- **Статус:** ✅ Implemented (по построению)
- **Реализация:** в кластере лабораторий нет API идентичности; LabGroupClient управляет только WG-credentials.
- **Что считать выполненным:** в namespace группы нет JWT, нет challenge-token-приватника (он только на платформе).

---

### Pool allocator

#### REQ-OP-070: Bitmap-аллокация с overflow в новый Pool

- **Источник:** §11 «битмап; голый счётчик-битмап как самостоятельная истина — даёт утечки».
- **Статус:** ✅ Implemented
- **Реализация:** `pkg/api/pool/pool.go:AllocateIndex` — выбирает наиболее заполненный NotFull-пул (сортировка по
  `Status.Free` возр.), `bitmap.NextClear(0)`; при отсутствии → `createPool` (offset = prev.offset + prev.size).
- **Что считать выполненным:** аллокация заполняет один пул прежде чем создать новый; индексы монотонно растут; freed
  индексы могут переиспользоваться.

#### REQ-OP-071: Bit-0 резервируется при offset=0

- **Источник:** §11 (адресный 0 — сетевой адрес `/24`).
- **Статус:** ✅ Implemented
- **Реализация:** `pool.go:InitBitmap` — если `offset == 0` → `bm.Set(0)`, `free--`.
- **Что считать выполненным:** в первом пуле бит 0 занят при инициализации; `AllocateIndex` никогда не возвращает индекс
  0 если offset=0.

#### REQ-OP-072: Истина — у потребителей, пул — индекс

- **Источник:** §11 «бит занят ⇔ существует потребитель»; «после краша битмап пересобирается».
- **Статус:** ❌ Missing
- **Реализация:** нет процедуры пересборки битмапа из реальных потребителей при старте оператора.
- **Что считать выполненным:** при рестарте оператора есть процедура `reconcileBitmaps`, которая для каждого Pool
  сканирует потребителей (Connection.Status.VNI / LabGroupClient.Status.AssignedIP / Lab subnet) и пересобирает bitmap,
  обнаруживая утечки.

#### REQ-OP-073: Сериализация выдачи (MaxConcurrentReconciles=1 + leader election)

- **Источник:** §11 «`MaxConcurrentReconciles=1` на контроллере, выбирающем значение, + leader election».
- **Статус:** ⚠️ Partial
- **Реализация:** controller-runtime по умолчанию использует `MaxConcurrentReconciles=1` (не задано явно);
  `enableLeaderElection=false` по умолчанию (`cmd/main.go:74`).
- **Что считать выполненным:** в production-манифесте leader election включён; reconcilers, выдающие индексы, имеют
  явный `MaxConcurrentReconciles=1` в `SetupWithManager`.
- **Заметки/gap:** дефолты сейчас совпадают со спецификацией только при single-replica deployment. Несколько реплик без
  leader election сломают атомарность выдачи.

#### REQ-OP-074: Ленивое освобождение (буфер из одного пустого пула)

- **Источник:** §11 «Освобождение — лениво (буфер из одного пустого пула, чтобы не дребезжать)».
- **Статус:** ❌ Missing
- **Реализация:** нет; `ReleaseIndex` не удаляет пустые Pool-ы.
- **Что считать выполненным:** при освобождении последнего индекса в Pool, если в namespace уже есть другой пустой Pool,
  текущий удаляется (буфер = 1 пустой).

---

### Поток разворачивания (§14)

#### REQ-OP-080: LabGroup → namespace → keypair → Deployment → registration → IP-pool

- **Источник:** §14 «`LabGroup` создан → namespace + VPN-сервер + регистрация `pubkey→backend` в демуксе + IP-пул».
- **Статус:** ⚠️ Partial
- **Реализация:** `labgroup_controller.go:Reconcile` — namespace ✅, keypair ✅, Deployment ✅, два pool-а ✅. Регистрация в
  демуксе — отсутствует ([REQ-OP-013](#req-op-013-регистрация-pubkeybackend-в-демукс)).
- **Что считать выполненным:** на Ready-фазе LabGroup демукс уже знает pubkey→backend, и клиент может коннектиться.

#### REQ-OP-081: Lab → VNI/subnet alloc → Device/Connection materialize → VPN port → web Service+NP

- **Источник:** §14 «`Lab` создан → VNI и подсети, материализует Device/Connection, поднимает порт-на-лабу на
  VPN-сервере, для `exposure.web` — Service + NetworkPolicy».
- **Статус:** ⚠️ Partial
- **Реализация:** subnet ✅, device materialize ✅, connection materialize ✅, web Service+NP ✅. Поднятие порта-на-лабу на
  VPN-сервере — задача VPN-контроллера (см. `vpn.md`), не оператора.
- **Что считать выполненным:** все шаги выполнены; Lab.status.access наполнен.

#### REQ-OP-082: Ready → status.access наполнен → платформа подставляет адреса

- **Источник:** §14 «`Lab.phase = Ready` → status наполнен → платформа подставляет реальные адреса».
- **Статус:** ❌ Missing
- **Реализация:** см. [REQ-OP-029](#req-op-029-labstatusaccess--публичные-url-после-ready).
- **Что считать выполненным:** платформа читает `lab.status.access[*].url` и видит реальные домены задач.

---

## Outstanding gaps

Сводный список открытых пунктов (отсортирован по приоритету):

### Блокеры (без них v1 не работает)

- ❌ **REQ-OP-011** Глобальная уникальность LabGroup pubkey — иначе демукс ломается.
- ❌ **REQ-OP-013** Регистрация pubkey→backend в демукс — без этого VPN недоступен.
- ❌ **REQ-OP-014** LabGroup.status.vpn.endpoint — без него клиент не знает куда коннектиться.
- ❌ **REQ-OP-029** Lab.status.access — без него платформа не знает URL заданий.

### Важно (нарушает изоляцию или корректность)

- ❌ **REQ-OP-026** Валидация broadcast-домена VPN↔internet.
- ❌ **REQ-OP-027** Валидация ≤1 DHCP/домен.
- ❌ **REQ-OP-062** Уникальность pubkey клиента в группе.
- ⚠️ **REQ-OP-063** Полный Secret клиента (нет endpoint/serverPublicKey/allowedIPs).
- ⚠️ **REQ-OP-073** MaxConcurrentReconciles=1 + leader election (зависит от deployment).

### Желательно (надёжность, удобство)

- ⚠️ **REQ-OP-002** owner-ref Lab→LabGroup (cross-namespace).
- ⚠️ **REQ-OP-003** admission webhook для Device/Connection read-only.
- ⚠️ **REQ-OP-020** immutability Lab.spec после Ready.
- ⚠️ **REQ-OP-022** VNI pool — фактически namespaced, а не cluster-scoped (требует bootstrap-логики).
- ⚠️ **REQ-OP-028** Уточнить «вторая нога» для web-устройств.
- ⚠️ **REQ-OP-040** VM type — заглушка как container.
- ⚠️ **REQ-OP-042** Device.status.nodeAddress не заполняется.
- ⚠️ **REQ-OP-050** Connection MaxItems=2.
- ❌ **REQ-OP-072** Пересборка bitmap из потребителей при старте.
- ❌ **REQ-OP-074** Lazy pool release.
- ❌ **REQ-OP-010** Проверка соответствия pub↔priv в переданном Secret.

---

## Кросс-ссылки

- VPN-сервер развёртывание и регистрация: см. [vpn.md](./vpn.md)
- Программирование OVS и DHCP-сервер: см. [node-agent.md](./node-agent.md)
- Демукс watch на LabGroup: см. [proxy.md](./proxy.md)
- Gateway конфигурация per-lab: см. [gateway.md](./gateway.md)
