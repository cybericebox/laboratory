# CNI Plugins — спецификация требований

> Источник: `cybericebox-spec-v1.md` §7 (meta-CNI, инвариант eth0, три случая annotation), §1 (термины), общий архитектурный принцип «CNI не имеет прямого доступа к K8s API» — все обращения через gRPC node-agent.

## Назначение

Два CNI-плагина живут на каждой ноде кластера лабораторий:

- **`cni-gate`** — primary meta-CNI в `10-cybericebox.conflist`. Bootstrap pod-а: всегда возвращает `eth0` в `Result` (реальный или sentinel-dummy), делегирует дефолтную сеть кластерному CNI (`bridge`) или скипает её по pod-аннотации.
- **`cni-ovs`** — отдельный CNI-binary для лабовых портов. Читает аннотацию `network.cybericebox.com/networks` (формат `conn1@eth0,conn2@eth1`), для каждой записи делает gRPC `AddPort` к node-agent.

Оба должны брать данные о pod-е (аннотации) **только** через gRPC node-agent — прямой доступ к K8s API запрещён архитектурно.

## Карта реализации

| Файл | Роль |
|------|------|
| `cmd/cni-gate/main.go` | meta-CNI: parse аннотации `network.cybericebox.com/default`, делегирование, dummy eth0 |
| `cmd/cni-ovs/main.go` | лабовые порты: parse `network.cybericebox.com/networks`, AddPort/DeletePort через gRPC |
| `api/node/v1/*.pb.go` | gRPC-биндинги NodeAgent (AddPort, DeletePort, GetPodAnnotation) |
| `config/cni/10-cybericebox.conflist` | conflist с cni-gate как primary, делегат `bridge` |

Статус-таксономия — см. [operator.md](./operator.md#условные-обозначения-статуса).

---

## Требования

### Общие архитектурные

#### REQ-CNI-001: Запрет прямого доступа к Kubernetes API из CNI-плагинов
- **Источник:** общая архитектура («Синеплагин не должен иметь доступ напрямую к губернете SAP... только через демона»).
- **Статус:** ⚠️ Partial
- **Реализация:** `cmd/cni-gate/main.go:getPodAnnotation` использует gRPC `NodeAgent.GetPodAnnotation` ✅; `cmd/cni-ovs/main.go:getPodAnnotation` (строки 163-181) **читает K8s API напрямую через kubeconfig** `/etc/cni/net.d/cybericebox-kubeconfig.conf` ❌.
- **Что считать выполненным:** оба плагина обращаются за аннотациями только через gRPC node-agent; в коде нет `clientcmd.BuildConfigFromFlags`, нет `kubernetes.NewForConfig`.
- **Заметки/gap:** cni-ovs должен повторить путь cni-gate: gRPC `GetPodAnnotation` на сокете node-agent. Сейчас он держит kubeconfig (которого даже нет в DaemonSet'е node-agent — initContainer его не создаёт после исправления). Эта функция в рантайме упадёт.

#### REQ-CNI-002: Один gRPC-сокет node-agent на ноде
- **Источник:** §7 (CNI говорит с локальным агентом).
- **Статус:** ✅ Implemented
- **Реализация:** обе плагин-binary дефолтят на `/run/cybericebox/node-agent.sock` (cni-ovs `defaultGRPCSock`, cni-gate `defaultAgentSocket`). Conflist `10-cybericebox.conflist` явно задаёт `agentSocket: /run/cybericebox/node-agent.sock`.
- **Что считать выполненным:** оба плагина при пустом параметре конфига коннектятся к одному и тому же hostPath-сокету.
- **Заметки/gap:** `defaultAgentSocket` в cni-gate указан как `/run/openvswitch/node-agent.sock` (строка 24), но conflist всегда переопределяет это значение. Минорное несоответствие — устаревший дефолт.

#### REQ-CNI-003: Insecure gRPC по Unix-socket
- **Источник:** локальный IPC, TLS не нужен.
- **Статус:** ✅ Implemented
- **Реализация:** обе плагин-binary: `grpc.NewClient("unix://"+sock, grpc.WithTransportCredentials(insecure.NewCredentials()))`.
- **Что считать выполненным:** соединения устанавливаются без TLS; UDS-разрешения управляют доступом.

---

### cni-gate (meta-CNI)

#### REQ-CNI-010: Primary в conflist
- **Источник:** §7 «meta-CNI становится primary на всех нодах; delegate-ветка тонкая».
- **Статус:** ✅ Implemented
- **Реализация:** `config/cni/10-cybericebox.conflist` содержит ровно один plugin `cni-gate` с делегатом `bridge`.
- **Что считать выполненным:** kubelet вызывает только cni-gate; cni-gate сам решает, делегировать ли кластерному CNI.

#### REQ-CNI-011: Всегда возвращать `eth0` в Result
- **Источник:** §7 «Инвариант: всегда возвращает `eth0` в Result (реально настроен или sentinel)».
- **Статус:** ✅ Implemented
- **Реализация:** `cmd/cni-gate/main.go:cmdADD`:
  - `skipDelegate=true` (annotation=`""`): создаётся dummy eth0 через `createDummyEth0`, возвращается пустой `Result{CNIVersion: ...}`.
  - `skipDelegate=false, targetIface="eth0"`: делегирование, `eth0` приходит из delegate.
  - `skipDelegate=false, targetIface!="eth0"`: делегирование → переименование eth0→custom → создание dummy eth0.
- **Что считать выполненным:** kubelet всегда видит `eth0` в результате; pod.status.podIP заполняется (либо реальным, либо dummy 0.0.0.0).

#### REQ-CNI-012: Три случая аннотации `network.cybericebox.com/default`
- **Источник:** §7 «Три случая дефолтной сети: аннотация пропущена → eth0 делегируется... прочерк → дефолта нет... задана с кастомным именем → дефолт на кастомном порту».
- **Статус:** ✅ Implemented
- **Реализация:** `cmd/cni-gate/main.go:parseDefaultAnnotation`:
  - нет аннотации (`noAnnotation=true`) или `="eth0"` → `(skipDelegate=false, targetIface="eth0")`;
  - `=""` (прочерк) → `(skipDelegate=true, "")`;
  - `="custom"` → `(skipDelegate=false, targetIface="custom")`.
- **Что считать выполненным:** все три варианта поведения видны на тестах; кастомное имя приводит к интерфейсу-с-IP + dummy eth0.

#### REQ-CNI-013: Делегирование кластерному CNI (bridge)
- **Источник:** §7 (meta-CNI делегирует default-сеть).
- **Статус:** ✅ Implemented
- **Реализация:** `main.go:cmdADD` — `invoke.DelegateAdd(ctx, dt, marshalDelegate(conf), nil)`; `conf.Delegate` содержит конфиг `bridge` из conflist; `cmdDEL` — `invoke.DelegateDel`.
- **Что считать выполненным:** при annotation отсутствует или `="eth0"` pod получает IP из delegate IPAM (`host-local` на `10.244.0.0/16`).

#### REQ-CNI-014: Получение pod-аннотации через gRPC node-agent
- **Источник:** §7 + общий принцип REQ-CNI-001.
- **Статус:** ✅ Implemented
- **Реализация:** `main.go:getPodAnnotation` использует `nodev1.NewNodeAgentClient(conn).GetPodAnnotation(...)`.
- **Что считать выполненным:** cni-gate коннектится к Unix-сокету, делает один RPC за время CNI ADD; latency приемлемый (<50ms).

#### REQ-CNI-015: Парсинг CNI_ARGS для pod-identity
- **Источник:** общая CNI-конвенция.
- **Статус:** ✅ Implemented
- **Реализация:** `main.go:parsePodArgs` ищет `K8S_POD_NAMESPACE`, `K8S_POD_NAME` (без UID — для cni-gate этого достаточно).
- **Что считать выполненным:** при стандартном вызове kubelet извлекаются ns и name; пустые → ранний возврат без обращения к node-agent.

#### REQ-CNI-016: Idempotent CHECK
- **Источник:** CNI spec — CHECK обязательный для CNI 1.0.
- **Статус:** ⚠️ Partial
- **Реализация:** `cmdCHECK` — no-op (`return nil`).
- **Что считать выполненным:** CHECK реально проверяет соответствие сети заявленному результату (intf eth0 существует, IP назначен и т.п.).
- **Заметки/gap:** заглушка. Для v1 допустимо.

---

### cni-ovs (лабовые порты)

#### REQ-CNI-030: Парсинг аннотации `network.cybericebox.com/networks`
- **Источник:** §7 (CNI-флоу для multi-network attach).
- **Статус:** ✅ Implemented
- **Реализация:** `cmd/cni-ovs/main.go:parseNetworksAnnotation` — формат `conn1@eth0,conn2@eth1`; пустые значения и записи без `@` пропускаются.
- **Что считать выполненным:** все запятые-разделённые записи превращаются в `[]netAttachment{Connection, Interface}`.

#### REQ-CNI-031: AddPort для каждого attachment через gRPC
- **Источник:** §7 (агент создаёт OVS-порт + move в netns).
- **Статус:** ✅ Implemented
- **Реализация:** `cmdADD` итерирует attachments, для каждой → `client.AddPort(AddPortRequest{PodUid, Connection, InterfaceName, Namespace, NetnsPath})`.
- **Что считать выполненным:** для аннотации `c1@i1,c2@i2` создаются два OVS-порта, два интерфейса внутри пода с именами `i1`, `i2`.

#### REQ-CNI-032: Rollback при частичной ошибке
- **Источник:** общая CNI-практика: при ошибке ADD очистить уже созданные ресурсы.
- **Статус:** ⚠️ Partial
- **Реализация:** при ошибке после хотя бы одного успешного AddPort — `client.DeletePort(podUID)` (удаляет все порты этого пода).
- **Что считать выполненным:** при N успешных + 1 ошибке остаётся 0 портов; pod не остаётся в полу-настроенном состоянии.
- **Заметки/gap:** `DeletePort` удаляет **все** порты пода, в т.ч. созданные ранее cni-ovs в другом порядке. Если несколько вызовов cni-ovs идут параллельно (что не должно случаться, но всё же), rollback может удалить чужие порты. Маловероятный сценарий — допустимо.

#### REQ-CNI-033: DEL — best-effort DeletePort
- **Источник:** §7 (CNI DEL вызывает агент на удаление).
- **Статус:** ✅ Implemented
- **Реализация:** `cmdDEL` — игнорирует ошибки коннекта/RPC (best-effort); вызывает `DeletePort(podUID)`.
- **Что считать выполненным:** удаление пода всегда вызывает DEL; node-agent удаляет все OVS-порты по podUID.

#### REQ-CNI-034: Получение pod-аннотации через gRPC node-agent
- **Источник:** REQ-CNI-001.
- **Статус:** ❌ Missing
- **Реализация:** `getPodAnnotation` использует kubeconfig + k8s API directly (см. REQ-CNI-001 заметки).
- **Что считать выполненным:** функция переписана по аналогии с `cni-gate`: открывает gRPC-канал, вызывает `NewNodeAgentClient(conn).GetPodAnnotation`.

#### REQ-CNI-035: Парсинг K8S_POD_UID из CNI_ARGS
- **Источник:** §7 (AddPort требует pod UID для трекинга).
- **Статус:** ✅ Implemented
- **Реализация:** `parsePodArgs` извлекает namespace, name **и** uid (в отличие от cni-gate).
- **Что считать выполненным:** AddPortRequest.PodUid заполнен реальным UID; node-agent может ассоциировать порты с pod-ом.

#### REQ-CNI-036: Подключение через conflist или multus
- **Источник:** §7 (cni-ovs — не primary; вызывается отдельно).
- **Статус:** 🔍 Needs verification
- **Реализация:** `10-cybericebox.conflist` содержит **только** `cni-gate`. Где вызывается `cni-ovs`?
- **Что считать выполненным:** есть явный механизм запуска cni-ovs — либо отдельный NetworkAttachmentDefinition (multus), либо secondary entry в conflist, либо cni-ovs вызывается из delegate в cni-gate, либо node-agent вызывает binary напрямую.
- **Заметки/gap:** в текущем conflist `cni-ovs` нигде не упоминается; binary копируется в `/opt/cni/bin/`, но никто не зовёт. Возможно работает «через multus dynamic-networks-controller» (§7 упоминает шаблон) — это нужно подтвердить инфраструктурой деплоя.

---

### Согласование между плагинами и node-agent

#### REQ-CNI-040: PodUID в RPC соответствует kubelet-pod UID
- **Источник:** §7 (агент кеширует podUID → netns; удаление по podUID).
- **Статус:** ✅ Implemented
- **Реализация:** kubelet передаёт UID в `CNI_ARGS=K8S_POD_UID=...`; cni-ovs парсит, отправляет в AddPortRequest.
- **Что считать выполненным:** node-agent.grpc.go хранит `podNetNS[podUid]`, `podPorts[podUid]` по тому же ключу; DEL находит и удаляет.

#### REQ-CNI-041: NetnsPath передаётся из CNI args
- **Источник:** §7 (CNI знает netns; агент в неё мостится).
- **Статус:** ✅ Implemented
- **Реализация:** `args.Netns` → `AddPortRequest.NetnsPath` → `node-agent` использует `netns.GetFromPath`.
- **Что считать выполненным:** netns путь из CNI ADD сохраняется; node-agent двигает интерфейс именно в этот netns.

#### REQ-CNI-042: Имя интерфейса внутри netns совпадает с запрошенным
- **Источник:** §7 (после переименования внутри netns).
- **Статус:** ✅ Implemented
- **Реализация:** `AddPortRequest.InterfaceName` → `node-agent.AddPort` → `RenameInNetNS` после `MoveToNetNS`.
- **Что считать выполненным:** в pod-е виден интерфейс с именем `eth1` (или другим заявленным), не `pXXXXX` (имя OVS-порта).

---

### conflist

#### REQ-CNI-050: cniVersion 1.0.0
- **Источник:** §7 (CNI 1.0).
- **Статус:** ✅ Implemented
- **Реализация:** `10-cybericebox.conflist` — `"cniVersion": "1.0.0"`.

#### REQ-CNI-051: Delegate: bridge + IPAM host-local
- **Источник:** §7 (bootstrap дефолтной сети как у Multus).
- **Статус:** ✅ Implemented
- **Реализация:** delegate = `{type: bridge, bridge: cni0, isGateway: true, ipMasq: true, ipam: {type: host-local, subnet: 10.244.0.0/16, routes: [{dst: 0.0.0.0/0}]}}`.
- **Что считать выполненным:** для подов без специальной аннотации дефолтная eth0 — bridge `cni0`, IP из `10.244.0.0/16`.

#### REQ-CNI-052: agentSocket прописан явно
- **Источник:** REQ-CNI-002.
- **Статус:** ✅ Implemented
- **Реализация:** в plugin-entry `cni-gate` поле `agentSocket: /run/cybericebox/node-agent.sock`.

---

## Outstanding gaps

### Блокеры

- ❌ **REQ-CNI-034 + REQ-CNI-001** — cni-ovs читает K8s API напрямую через kubeconfig. Архитектурное нарушение. Нужно переписать `getPodAnnotation` через gRPC `NodeAgent.GetPodAnnotation` (cni-gate уже сделан).
- 🔍 **REQ-CNI-036** — неясно, как cni-ovs вообще вызывается. В conflist его нет; binary копируется на ноду, но никто не зовёт. Без чёткого механизма (multus / NAD / delegate-chain) лабовые порты не подключаются.

### Желательно

- ⚠️ **REQ-CNI-016** — cni-gate `cmdCHECK` no-op (приемлемо для v1).
- ⚠️ **REQ-CNI-032** — rollback `DeletePort` агрессивный (удаляет все порты пода).
- ⚠️ **REQ-CNI-002** — устаревший дефолт `defaultAgentSocket` в cni-gate (`/run/openvswitch/...`). Незначительно: conflist перекрывает.

---

## Кросс-ссылки

- gRPC NodeAgent сервис (server-side): [node-agent.md#req-na-080](./node-agent.md#req-na-080-grpc-на-host-path-unix-сокете)
- OVS port creation, netns moving: [node-agent.md#req-na-020](./node-agent.md#req-na-020-internal-порт-для-подключения-подovs-домен)
- Pod annotation `network.cybericebox.com/networks` — кто/где её ставит: [operator.md](./operator.md) (должен заполнять Lab/Device reconciler при создании Pod)
