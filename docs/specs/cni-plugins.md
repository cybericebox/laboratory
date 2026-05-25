# CNI Plugins — спецификация требований

> Источник: `cybericebox-spec-v1.md` §7 (meta-CNI, инвариант eth0, три случая annotation), §1 (термины), общий архитектурный принцип «CNI не имеет прямого доступа к K8s API» — все обращения через gRPC node-agent.

## Назначение

Один CNI-плагин на каждой ноде:

- **`cni-gate`** — primary meta-CNI в `10-cybericebox.conflist`. Bootstrap pod-а: всегда возвращает `eth0` в `Result` (реальный или sentinel-dummy), делегирует дефолтную сеть кластерному CNI (`bridge`) или скипает её по pod-аннотации.

Берёт данные о pod-е (аннотации) **только** через gRPC node-agent — прямой доступ к K8s API запрещён архитектурно.

> **Архитектурное решение (Sprint 2):** ранее существовавший `cmd/cni-ovs/` удалён.
> Лабовые порты подключаются не через CNI, а через `cmd/node-agent/connection_reconciler.go`
> на CRD-watch — `Connection` материализуется → `AddInternalPort` + `MoveToNetNS` + flows,
> без зависимости от pod-аннотации и CNI-вызова. Это соответствует §7 «инкрементальные
> изменения скоупнутой операцией по одной сети (как multus-dynamic-networks-controller),
> не повторным прогоном всего чейна».

## Карта реализации

| Файл | Роль |
|------|------|
| `cmd/cni-gate/main.go` | meta-CNI: parse аннотации `network.cybericebox.com/default`, делегирование, dummy eth0 |
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

### Лабовые порты — через ConnectionReconciler, не CNI

> **Resolved (Sprint 2):** требования REQ-CNI-030..036 удалены вместе с
> `cmd/cni-ovs/`. Лабовые порты материализуются `cmd/node-agent/connection_reconciler.go`
> по watch на `Connection`-CRD: при появлении endpoint, привязанного к локальной ноде,
> агент сам вызывает `AddInternalPort` + `MoveToNetNS` + конфигурирует MAC/IP + ставит
> egress/ingress flow. Тот же путь поднимает `lab-<n>` / `gw-<n>` для VPN/Gateway-pod'ов
> через `lab_reconciler.go`.

См.:
- [node-agent.md REQ-NA-090](./node-agent.md#req-na-090-connectionreconciler--материализация-портов-и-flows)
- [node-agent.md REQ-NA-091](./node-agent.md#req-na-091-labifacereconciler--lab-name--gw-name-для-vpngateway-podов)

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

### Желательно

- ⚠️ **REQ-CNI-016** — cni-gate `cmdCHECK` no-op (приемлемо для v1).
- ⚠️ **REQ-CNI-002** — устаревший дефолт `defaultAgentSocket` в cni-gate (`/run/openvswitch/...`). Незначительно: conflist перекрывает.

---

## Кросс-ссылки

- gRPC NodeAgent сервис (server-side): [node-agent.md#req-na-080](./node-agent.md#req-na-080-grpc-на-host-path-unix-сокете)
- OVS port creation, netns moving: [node-agent.md#req-na-020](./node-agent.md#req-na-020-internal-порт-для-подключения-подovs-домен)
- Лабовые порты — через node-agent ConnectionReconciler: [node-agent.md REQ-NA-090](./node-agent.md#req-na-090-connectionreconciler--материализация-портов-и-flows)
