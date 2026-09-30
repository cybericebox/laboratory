# VPN Server — спецификация требований

> Источник: `cybericebox-spec-v1.md` §4 (VPN-вход и демукс — VPN-серверная часть), §9 (синглтон VPN в лабе), §12 (
> LabGroupClient pairing), §13 (изоляция лаб через FORWARD-политику), §14 (поток разворачивания).

## Назначение

VPN-сервер — singleton-pod в namespace LabGroup (`labgroup-<UID>`). Реализует WireGuard endpoint группы: ключи в
Secret (приватный никогда не покидает кластер), pubkey используется демуксом как routing-key. Внутри сервера:

- Конфигурируется WireGuard-интерфейс `wg0` приватным ключом сервера.
- Добавляются peer-ы по LabGroupClient (CRD-watch внутри namespace).
- Через одну OVS-ногу (`lab-<labName>`, создаётся node-agent-ом снаружи) сервер маршрутизирует входящий VPN-трафик в
  каждую активную лабу.
- Запускается единственный FORWARD-policy: разрешён user↔lab, запрещён lab↔lab.
- Периодически собирается peer-статистика (handshake, RX/TX) и пишется в `LabGroupClient.status.statistics`.

Сервер **не** регистрирует сам себя в демуксе — это задача оператора (
см. [operator.md REQ-OP-013](./operator.md#req-op-013-регистрация-pubkeybackend-в-демукс)).

## Карта реализации

| Файл                         | Роль                                                                                                     |
|------------------------------|----------------------------------------------------------------------------------------------------------|
| `cmd/vpn/main.go`            | Bootstrap: WG init, iptables FORWARD policy, manager с namespace-scope, 2 reconcilers, stats-goroutine   |
| `cmd/vpn/config.go`          | env-config: PRIVATE_KEY, NAMESPACE, LISTEN_PORT, STATS_INTERVAL; hardcoded 10.8.0.0/24, 10.8.0.0/16, wg0 |
| `cmd/vpn/wireguard.go`       | WGManager: Init, AddPeer, RemovePeer, Device — wrapper над `wgctrl`                                      |
| `cmd/vpn/reconciler.go`      | LabGroupClientReconciler (peer add/remove) + LabVPNReconciler (lab route) + runStats                     |
| `cmd/vpn/routes.go`          | addLabRoute / delLabRoute / waitForInterface через netlink                                               |
| `cmd/vpn/iptables.go`        | FORWARD-policy 4 правила (ESTABLISHED, src/dst clientSubnet, DROP)                                       |
| `internal/ovsnames/names.go` | `LabIfaceName(labName)` — стабильное имя `lab-<name>` ≤ 15 chars                                         |

Статус-таксономия — см. [operator.md](./operator.md#условные-обозначения-статуса).

---

## Требования

### Singleton и базовая инфраструктура

#### REQ-VPN-001: Один pod на группу

- **Источник:** §4 «Один на группу (своя пара ключей)», §9 «1 экземпляр».
- **Статус:** ✅ Implemented
- **Реализация:** оператор создаёт Deployment×1 с `app=vpn` в namespace
  группы ([REQ-OP-012](./operator.md#req-op-012-vpn-сервер-развёрнут-как-deployment1)); сам бинарь предполагает
  single-instance (manager без leader election).
- **Что считать выполненным:** в `labgroup-<UID>` существует ровно один Pod `app=vpn`; перезапуск не приводит к двум
  активным.

#### REQ-VPN-002: Namespace-scoped manager

- **Источник:** §10 (LabGroup = граница арендатора), §12 (один VPN видит только своих клиентов).
- **Статус:** ✅ Implemented
- **Реализация:** `main.go` — `ctrl.NewManager(..., Cache.DefaultNamespaces: {cfg.Namespace: {}})`;
  LabGroupClientReconciler и LabVPNReconciler видят объекты только из namespace LabGroup.
- **Что считать выполненным:** VPN-сервер группы A не наблюдает LabGroupClient/Lab из группы B; cross-group leak
  невозможен.

#### REQ-VPN-003: PRIVATE_KEY из Secret через envFrom

- **Источник:** §10 (приватник только в Secret, никогда не в spec).
- **Статус:** ✅ Implemented
- **Реализация:** `config.go:loadConfig` — `os.Getenv("PRIVATE_KEY")`; оператор смонтировал Secret `vpn-server-keypair`
  через
  `envFrom` ([labgroup_controller.go:ensureVPNDeployment](../../internal/controller/laboratory/labgroup_controller.go)).
- **Что считать выполненным:** приватный ключ не появляется в `spec.containers[].env`, виден только через Secret;
  перезапуск pod-а сохраняет идентичность сервера.

#### REQ-VPN-004: WireGuard через wgctrl

- **Источник:** §4 «транспорт VPN — на базе протокола WireGuard».
- **Статус:** ✅ Implemented
- **Реализация:** `wireguard.go` использует `golang.zx2c4.com/wireguard/wgctrl` для `ConfigureDevice(wg0, ...)`.
- **Что считать выполненным:** интерфейс `wg0` существует в pod-е, имеет приватник, listen port 51820 (по умолчанию).
- **Заметки:** WireGuard kmod должен быть доступен на ноде; pod не использует userspace WireGuard. DaemonSet ноды
  отдельно.

#### REQ-VPN-005: ListenPort 51820 (по умолчанию)

- **Источник:** §4 «единый публичный порт».
- **Статус:** ✅ Implemented
- **Реализация:** `config.go` — `port := 51820`, override через `LISTEN_PORT` env.
- **Что считать выполненным:** все VPN-серверы кластера слушают один и тот же порт; демукс знает порт глобально.

---

### Две ноги (cluster + OVS fabric)

#### REQ-VPN-010: Кластерная нога pod-а (для демукс-форварда)

- **Источник:** §4 «Две ноги: (1) кластерная сеть — чтобы демукс форвардил входящий UDP на podIP».
- **Статус:** ✅ Implemented (по построению Pod)
- **Реализация:** Pod в `labgroup-<UID>` namespace получает обычный `eth0` от cluster CNI (через `cni-gate`
  делегирование bridge). `wg0` слушает на этой ноге.
- **Что считать выполненным:** `LabGroup.status.vpn.backend` = `<podIP>:<port>`; демукс отправляет UDP туда (
  см. [REQ-OP-013](./operator.md#req-op-013-регистрация-pubkeybackend-в-демукс)).

#### REQ-VPN-011: OVS-нога — `lab-<labName>` на каждую активную лабу

- **Источник:** §4 «(2) OVS-фабрика — выход в сети лаб»; §9 «1 порт, 1 сегмент в лабе».
- **Статус:** ✅ Implemented (создаётся снаружи)
- **Реализация:** `node-agent.lab_reconciler.go` создаёт internal OVS port `lab-<name>` и перемещает его в pod
  netns ([REQ-NA-091](./node-agent.md#req-na-091-labifacereconciler--lab-name--gw-name-для-vpngateway-podов)).
  VPN-сервер сам её не создаёт.
- **Что считать выполненным:** в pod-е VPN видим `ip link show lab-<labname>`; интерфейс UP.

#### REQ-VPN-012: Динамический порт-на-лабу без рестарта pod-а

- **Источник:** §4 «Лаба создалась → на VPN-сервере появляется порт в её сегмент (через собственную копию
  dynamic-networks-контроллера — патч аннотации, без рестарта пода)»; §7 «Живой hot-plug портов работающих подов… кроме
  порта-на-лабу VPN-сервера».
- **Статус:** ✅ Implemented
- **Реализация:** `lab_reconciler.go` в node-agent (watch Lab + Pod) при появлении новой Lab с
  `Spec.VPN.Enabled && Status.VPN.CIDR != ""` вызывает `OVS.AddInternalPort` + `MoveToNetNS` — без перезапуска pod-а.
- **Что считать выполненным:** новая Lab при её создании автоматически появляется как новый `lab-<n>` интерфейс в pod-е
  VPN; pod не перезапускается.

#### REQ-VPN-013: L3-роутер группы — `10.{vpn}.N.1` на каждом `lab-<n>`

- **Источник:** §4 «L3-роутер группы: `10.{vpn}.N.1` в каждой лабе и адрес в пользовательской подсети. Разводит трафик
  по портам-на-лабу по destination».
- **Статус:** ❌ Missing
- **Реализация:** `reconciler.go:LabVPNReconciler.Reconcile` вызывает `addLabRoute(link, cidr)` — только маршрут, но *
  *IP-адрес `10.8.N.1/24` на интерфейс не назначается**.
- **Что считать выполненным:** в pod-е VPN на интерфейсе `lab-<n>` выставлен адрес `10.8.N.1/24` (с broadcast/netmask);
  пакеты от клиента с dst `10.8.N.5` доходят через wg0→FORWARD→lab-N→локальная доставка лабовому устройству.
- **Заметки/gap:** без IP на интерфейсе ядро не маршрутизирует пакеты в направлении лабы. Это критический gap —
  VPN-связь до лабы не работает.

#### REQ-VPN-014: Пользовательский IP на `wg0` (входящая нога)

- **Источник:** §4 (по умолчанию VPN-сервер слушает на адресе из пользовательской подсети).
- **Статус:** 🔍 Needs verification
- **Реализация:** `Init` ставит только `PrivateKey + ListenPort`; IP на `wg0` не назначается через wgctrl. Возможно
  интерфейс создаётся через `ip link add wg0 type wireguard` где-то снаружи (initContainer / shell-script), но в коде Go
  этого нет.
- **Что считать выполненным:** на `wg0` присутствует адрес `10.8.0.1/24` (gateway пользовательской подсети); клиенты с
  `10.8.0.X/32` могут пинговать `10.8.0.1`.

---

### FORWARD-политика (§4, §13)

#### REQ-VPN-020: 4 инвариантных правила в filter/FORWARD

- **Источник:** §4 «Изоляция через FORWARD-политику (одно инвариантное правило)»; §13 «Лаба↔лаба (внутри группы): одно
  FORWARD-правило на VPN-роутере».
- **Статус:** ✅ Implemented
- **Реализация:** `iptables.go:SetupForwardPolicy` устанавливает:
    1. `-m conntrack --ctstate ESTABLISHED,RELATED -j ACCEPT`
    2. `-s 10.8.0.0/24 -j ACCEPT` (user→lab)
    3. `-d 10.8.0.0/24 -j ACCEPT` (lab→user, реверс-шеллы)
    4. `-j DROP` (lab↔lab и прочее).
- **Что считать выполненным:** клиент↔лаба работает в обе стороны; пакет от лабы А к лабе Б отбрасывается на
  VPN-сервере.

#### REQ-VPN-021: Cleanup правил при graceful shutdown

- **Источник:** общая практика.
- **Статус:** ✅ Implemented
- **Реализация:** `main.go:defer ipt.Cleanup()`; `iptables.go:Cleanup` удаляет все 4 правила.
- **Что считать выполненным:** SIGTERM → правила убраны; следующий старт `AppendUnique` идемпотентен.

---

### LabGroupClient — добавление WireGuard-peer (§12)

#### REQ-VPN-030: Watch LabGroupClient → AddPeer

- **Источник:** §12 (system добавляет peer по pubkey + assignedIP).
- **Статус:** ✅ Implemented
- **Реализация:** `LabGroupClientReconciler.Reconcile`: при `Spec.PublicKey != ""` и `Status.AssignedIP != ""` →
  `WG.AddPeer(pubKey, assignedIP)` (AllowedIPs = assignedIP).
- **Что считать выполненным:** после создания LabGroupClient peer виден в `wg show`; клиент с этим pubkey подключается.

#### REQ-VPN-031: Peer.AllowedIPs = assignedIP /32 (anti-spoofing)

- **Источник:** §4 (серверная сторона блокирует spoofing IP пира).
- **Статус:** ✅ Implemented
- **Реализация:** `wireguard.go:AddPeer` парсит `allowedIP` как CIDR (приходит `10.8.0.X/32`),
  `ReplaceAllowedIPs: true`, `AllowedIPs: [ipNet]`.
- **Что считать выполненным:** пакет от клиента с подменённым `src` отбрасывается WireGuard-ом (cryptokey routing).

#### REQ-VPN-032: Удаление peer при удалении LabGroupClient

- **Источник:** §12 (lifecycle).
- **Статус:** ✅ Implemented
- **Реализация:** finalizer `cybericebox.com/vpn-peer`; на DEL → `RemovePeer(pubKey)`, затем убирается finalizer.
- **Что считать выполненным:** удалённый клиент не может реконнектиться без пересоздания LabGroupClient.

#### REQ-VPN-033: PersistentKeepalive ~10–25 сек

- **Источник:** §4 «PersistentKeepalive ~10–25 сек — ускоряет восстановление при роуминге».
- **Статус:** ❌ Missing
- **Реализация:** `AddPeer` не задаёт `PersistentKeepaliveInterval`.
- **Что считать выполненным:** в `wg show` для каждого peer `persistent keepalive: 15 seconds`; при роуминге сессия
  восстанавливается за <30с.
- **Заметки/gap:** keepalive обычно ставится на стороне клиента, но spec прямо упоминает сервер. Безболезненно
  добавить — `PersistentKeepaliveInterval: 15 * time.Second`.

---

### LabVPN — маршрут к каждой активной лабе

#### REQ-VPN-040: Watch Lab → wait `lab-<n>` → addRoute

- **Источник:** §4 «Динамический порт-на-лабу… клиент при этом статичен: AllowedIPs = весь диапазон, разводка по лабам —
  забота сервера».
- **Статус:** ⚠️ Partial
- **Реализация:** `LabVPNReconciler.Reconcile` — при `lab.Spec.VPN.Enabled && lab.Status.VPN.CIDR != ""`:
  `LinkByName(lab-<n>)` → `addLabRoute(link, cidr)`.
- **Что считать выполненным:** в pod-е VPN маршрут `10.8.N.0/24 dev lab-<n>` существует; IP `10.8.N.1` присвоен
  интерфейсу (REQ-VPN-013).
- **Заметки/gap:** маршрут добавлен, но без IP на интерфейсе (REQ-VPN-013) маршрутизация не работает.

#### REQ-VPN-041: Lab.Status.VPN.Ready после успешного route

- **Источник:** §10 (статус наполняется по факту готовности).
- **Статус:** ✅ Implemented
- **Реализация:** `LabVPNReconciler.Reconcile` после `addLabRoute` → `lab.Status.VPN.Ready = true`, `Status().Update`.
- **Что считать выполненным:** Lab.status.vpn.ready=true виден в kubectl; оператор использует это для phase=Ready.

#### REQ-VPN-042: Cleanup маршрута + finalizer на DEL

- **Источник:** §14 (cleanup).
- **Статус:** ✅ Implemented
- **Реализация:** `reconcileDelete` — `delLabRoute(cidr)` + `RemoveFinalizer("cybericebox.com/vpn")`.
- **Что считать выполненным:** удаление Lab → маршрут удалён; finalizer снят → оператор может удалить Lab окончательно.

#### REQ-VPN-043: Wait for interface (poll, не subscribe)

- **Источник:** §14 (node-agent создаёт интерфейс асинхронно).
- **Статус:** ⚠️ Partial
- **Реализация:** `LabVPNReconciler.Reconcile` делает `LinkByName(...)`; при ошибке → `RequeueAfter: 5 * time.Second`.
  Файл `routes.go:waitForInterface` использует `LinkSubscribe` (event-driven), но в reconciler он **не вызывается** —
  только polling через requeue.
- **Что считать выполненным:** интерфейс обнаруживается за <5с после появления.
- **Заметки/gap:** `waitForInterface` определён, но не используется. Polling 5с приемлем для v1.

---

### Per-lab подсеть `10.{vpn}.N.0/24`

#### REQ-VPN-050: Подсеть берётся из Lab.Status.VPN.CIDR (детерминирована N)

- **Источник:** §2 «подсеть детерминированно выводится из N».
- **Статус:** ✅ Implemented
- **Реализация:** `LabVPNReconciler` читает `lab.Status.VPN.CIDR` (`10.8.N.0/24`), формат заполняется
  оператором ([REQ-OP-021](./operator.md#req-op-021-per-lab-подсети-108n024-и-109n024-общий-n)).
- **Что считать выполненным:** разные лабы получают разные N; маршруты в VPN-сервере не конфликтуют.

#### REQ-VPN-051: ClientSubnet 10.8.0.0/24, VPNSupernet 10.8.0.0/16

- **Источник:** §2 «10.{vpn}.N.0/24 — VPN-диапазон».
- **Статус:** ✅ Implemented
- **Реализация:** `config.go` hardcoded `_, clientSubnet, _ = net.ParseCIDR("10.8.0.0/24")`,
  `_, vpnSupernet, _ = net.ParseCIDR("10.8.0.0/16")`.
- **Что считать выполненным:** супернет 10.8.0.0/16 покрывает все потенциальные labCIDR (N=1..254); ClientSubnet
  используется в FORWARD-правилах.
- **Заметки:** значения захардкожены — соответствует «VPN-диапазон один на кластер» (§2).

---

### Stats (peer metrics)

#### REQ-VPN-060: Периодический опрос wgctrl и запись в LabGroupClient.status

- **Источник:** §12 (statistics видны платформе).
- **Статус:** ✅ Implemented
- **Реализация:** `runStats` goroutine, тикер `cfg.StatsInterval` (default 30s); `wg.Device()` → list LabGroupClient →
  match peer.PublicKey ↔ lgc.Spec.PublicKey → `Status.Statistics = {LastHandshake, RxBytes, TxBytes}`.
- **Что считать выполненным:** kubectl видит обновляющиеся статистики; нет лишних API-операций (best-effort).

---

#### REQ-VPN-061: Учёт обращений клиентов к лабораториям (flow accounting)

- **Источник:** `docs/LAB-TRAFFIC-ACCOUNTING.md` (в корне репозитория CyberICEBox), решение владельца 2026-09-30.
- **Статус:** ✅ Implemented (нужна проверка на живом кластере).
- **Реализация:** `internal/vpn/flowacct` читает conntrack по ctnetlink (`ti-mo/conntrack`) раз в 5 секунд в netns VPN-пода
  и считает только пары «клиент → сеть лаборатории группы»; всё остальное (внешние WireGuard-потоки с публичными адресами
  пользователей, probe, DHCP) отбрасывается до создания состояния. Агрегат на ключ (клиент `p-<uuid>`, лаборатория; любой адрес
  и порт сети лаборатории считаются доступом к ней): `attempts` (новые соединения), `firstSeen`, `lastSeen`, `firstResponded` (лаборатория ответила:
  `SEEN_REPLY`), пакеты и байты. Без временного ряда. В отчёте не больше 512 строк (`truncated`). После рестарта пода счётчики
  продолжаются с сохранённых в CR значений (`Collector.Resume`), потоки, начатые до последнего отчёта, повторно не считаются. Отчёт пишется раз в минуту в `LabTrafficReport/vpn`
  (status), там же `bootID` и покрытие `coveredFrom..coveredTo` (heartbeat, продвигается и без трафика; после пропуска
  дампа больше 3 интервалов `coveredFrom` начинается заново). Агент ретранслирует CR в `MonitoringUpdate.traffic`.
- **Байты:** нужны `nf_conntrack_acct` и `nf_conntrack_timestamp`; их включает privileged init-контейнер
  `conntrack-accounting` (основной контейнер остаётся непривилегированным). Без них попытки и ответы считаются, байты равны 0.
- **Что считать выполненным:** `kubectl get labtrafficreport vpn -n labgroup-...` показывает строки по клиентам после
  обращения к лаборатории; попытки на закрытый порт видны без `firstResponded`.
- **Ограничения:** отклонённые ACL пакеты (нет conntrack-записи) не видны; трафик за роутером внутри лабы виден только до
  устройства, обращённого к VPN; счётчик привязан к конфигу VPN, а не к человеку.

---

### Архитектурно: что VPN-сервер НЕ делает

#### REQ-VPN-070: Не регистрирует себя в демуксе

- **Источник:** §4 (источник правды pubkey→backend — CRD, watch у демукса).
- **Статус:** ✅ Implemented (по построению — нет такого кода)
- **Реализация:** VPN-сервер не знает ни про демукс, ни про публичный endpoint. Источник правды —
  `LabGroup.status.vpn.{publicKey, backend, endpoint}`, заполняется оператором.
- **Что считать выполненным:** в коде `cmd/vpn/` нет обращений к демукс-API/CRD; pubkey статичен через ConfigureDevice.

#### REQ-VPN-071: Не L2-мостит между лабами

- **Источник:** §9 «Не мост между сегментами (иначе обходит изоляцию); межсегментный роутинг внутри лабы — отдельный
  узел-роутер».
- **Статус:** ✅ Implemented
- **Реализация:** VPN-сервер только L3-роутер (через routes). FORWARD-DROP блокирует lab↔lab. Internal routing внутри
  лабы — отдельным `vm`/`container` устройством-роутером.
- **Что считать выполненным:** broadcast пакет из лабы A не доходит до лабы B; межсегментные пакеты внутри одной лабы
  идут через устройство-роутер, не VPN.

#### REQ-VPN-072: DHCP-сервер не в VPN

- **Источник:** §9 «Опционально встроенный DHCP-сервер».
- **Статус:** ❌ Missing
- **Реализация:** в коде VPN-сервера DHCP нет; реализация живёт в gateway ([REQ-GW-...](./gateway.md)).
- **Что считать выполненным:** для VPN-сегмента DHCP опционально включён через `Lab.Spec.VPN.DHCPServer` — где-то его
  нужно поднять. Если в VPN-pod — реализовать как в gateway; если у gateway — нужна логика «VPN-сегмент тоже
  обслуживается gateway-DHCP», что архитектурно странно.
- **Заметки/gap:** spec не уточняет где живёт DHCP для VPN-сегмента. По §9 VPN — синглтон со своими опциями, разумно
  ставить DHCP в VPN-pod. Сейчас не реализовано.

#### REQ-VPN-073: Без секции `DNS=` в клиентских конфигах

- **Источник:** §4 «Секции `DNS=` нет — иначе Apple-клиент ставит catch-all».
- **Статус:** 🔍 Needs verification
- **Реализация:** VPN-сервер не формирует клиентские конфиги. Это делает
  оператор ([REQ-OP-063](./operator.md#req-op-063-полный-secret-клиента)). Сейчас оператор пишет неполный Secret; полная
  строка `wg.conf` отсутствует. Когда добавят, важно не вписать `DNS=`.
- **Что считать выполненным:** в готовых `wg.conf`, выдаваемых клиенту, нет секции `DNS=`.

---

## Outstanding gaps

### Блокеры

- ❌ **REQ-VPN-013** — на интерфейсе `lab-<n>` не назначается IP `10.8.N.1/24`. Без него L3-маршрутизация в лабу не
  работает. **VPN-связь с лабой сломана по факту**.
- 🔍 **REQ-VPN-014** — на `wg0` тоже не назначается gateway IP клиентской подсети (`10.8.0.1/24`). Без него клиенты не
  могут пинговать сам VPN-сервер; пакеты от клиента к 10.8.0.1 проваливаются.

### Важно

- ❌ **REQ-VPN-033** — PersistentKeepalive отсутствует.
- ❌ **REQ-VPN-072** — DHCP-сервер для VPN-сегмента (если нужен) отсутствует.

### Желательно

- ⚠️ **REQ-VPN-040** — wait for interface через polling 5с (вместо `LinkSubscribe`).
- 🔍 **REQ-VPN-073** — нет полного клиентского `wg.conf` (зависит от оператора).

---

## Кросс-ссылки

- LabGroup → namespace → VPN Deployment + Secret +
  Pools: [operator.md REQ-OP-010..017](./operator.md#labgroup-reconciler)
- LabGroupClient → assignedIP, Secret: [operator.md REQ-OP-060..064](./operator.md#labgroupclient-reconciler)
- OVS port `lab-<n>` создаётся
  node-agent-ом: [node-agent.md REQ-NA-091](./node-agent.md#req-na-091-labifacereconciler--lab-name--gw-name-для-vpngateway-podов)
- Регистрация pubkey→backend в демуксе (оператор пишет, демукс
  читает): [operator.md REQ-OP-013](./operator.md#req-op-013-регистрация-pubkeybackend-в-демукс), [proxy.md](./proxy.md)
- Internet gateway — параллельная архитектура для egress: [gateway.md](./gateway.md)
