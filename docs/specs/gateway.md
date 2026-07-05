# Internet Gateway — спецификация требований

> Источник: `cybericebox-spec-v1.md` §9 (internet egress synglton), §2 (per-lab подсеть `10.{inet}.N.0/24`), §8 (
> DHCP-сервер на шлюзе), §14 (поток разворачивания).

## Назначение

Internet Gateway — singleton-pod в namespace LabGroup (`labgroup-<UID>`). Egress-only NAT для лаб: принимает исходящий
трафик из лабовых сегментов (`10.{inet}.N.0/24`), SNAT'ит в кластерную сеть → наружу. Обратные подключения из интернета
запрещены (нет проброса портов, conntrack защищает от unsolicited inbound).

Кроме NAT, может опционально запускать реальный DHCP-сервер на каждом лабовом сегменте (`Lab.Spec.Internet.DHCPServer`).

Архитектурно симметричен VPN-серверу: одна OVS-нога per-лаба, своя подсеть, gateway IP `10.9.N.1` на интерфейсе.
Отличие: трафик идёт **изнутри наружу** (а не наоборот), и есть DHCP.

## Карта реализации

| Файл                         | Роль                                                                   |
|------------------------------|------------------------------------------------------------------------|
| `cmd/gateway/main.go`        | Bootstrap: IPTables manager, DHCP manager, namespace-scoped controller |
| `cmd/gateway/config.go`      | env-config: NAMESPACE, EXTERNAL_INTERFACE (default `eth0`)             |
| `cmd/gateway/reconciler.go`  | LabGatewayReconciler: assign gateway IP → SNAT → DHCP start            |
| `cmd/gateway/iptables.go`    | AddMasquerade / DelMasquerade (POSTROUTING SNAT)                       |
| `cmd/gateway/dhcp.go`        | DHCPManager: per-lab DHCPv4 server, OFFER/ACK с lease 24h              |
| `cmd/gateway/routes.go`      | nextIP, assignGatewayIP — назначение `10.9.N.1/24` на интерфейс        |
| `internal/ovsnames/names.go` | `LabGWIfaceName(labName)` — стабильное имя `gw-<name>` ≤ 15 chars      |

Статус-таксономия — см. [operator.md](./operator.md#условные-обозначения-статуса).

---

## Требования

### Базовая инфраструктура

#### REQ-GW-001: Один pod на группу

- **Источник:** §9 «1 экземпляр, egress-only».
- **Статус:** ❌ Missing
- **Реализация:** Deployment gateway **не создаётся оператором**.
  `internal/controller/laboratory/labgroup_controller.go:ensureVPNDeployment` создаёт только VPN-pod; gateway-pod не
  создаётся ни в LabGroup-reconciler, ни в Lab-reconciler.
- **Что считать выполненным:** в `labgroup-<UID>` существует ровно один Pod `app=gateway`; оператор разворачивает его на
  этапе ensure*.
- **Заметки/gap:** есть готовый бинарь, но никто его не разворачивает в кластере. Аналогично VPN-deployment должен
  ставиться LabGroup-reconciler-ом.

#### REQ-GW-002: Namespace-scoped manager

- **Источник:** §10 (LabGroup = граница арендатора).
- **Статус:** ✅ Implemented
- **Реализация:** `main.go` — `ctrl.NewManager(..., Cache.DefaultNamespaces: {cfg.Namespace: {}})`; LabGatewayReconciler
  видит Lab из одного namespace.
- **Что считать выполненным:** Gateway группы A не наблюдает Lab из группы B.

#### REQ-GW-003: EXTERNAL_INTERFACE env (default `eth0`)

- **Источник:** §9 (egress наружу через кластерную сеть).
- **Статус:** ✅ Implemented
- **Реализация:** `config.go` — `EXTERNAL_INTERFACE` env, fallback `eth0`.
- **Что считать выполненным:** SNAT правило ссылается на правильный интерфейс pod-а, через который трафик уходит в
  cluster network.

#### REQ-GW-004: Watch Lab CRD внутри namespace

- **Источник:** §10 + §14 (Lab → внесение per-lab правил).
- **Статус:** ✅ Implemented
- **Реализация:** `reconciler.go:SetupWithManager` — `For(&Lab{})`.
- **Что считать выполненным:** новая Lab с `Spec.Internet.Enabled` триггерит reconcile в течение секунд.

---

### Per-lab OVS-нога

#### REQ-GW-010: OVS-нога `gw-<labName>` (создаётся node-agent-ом)

- **Источник:** §9 (gateway — «VPN наоборот: порт-на-лабу»); §7 (нога создаётся node-agent-ом).
- **Статус:** ⚠️ Partial
- **Реализация:** `node-agent.lab_reconciler.go:Reconcile` при
  `lab.Spec.Internet.Enabled && lab.Status.Internet.CIDR != ""` вызывает
  `ensureLabIface(..., "gateway", ovsnames.LabGWIfaceName(lab.Name))` — создаёт `gw-<labname>` в gateway-pod netns ✅.
  Однако сам gateway reconciler ищет интерфейс под **другим** именем (см. REQ-GW-011).
- **Что считать выполненным:** в gateway-pod-е виден `gw-<labname>` с поднятым линком; gateway reconciler находит его.

#### REQ-GW-011: Несоответствие имени интерфейса

- **Источник:** §9 (одна нога per-lab, имя стабильное).
- **Статус:** ❌ Missing (блокер)
- **Реализация:** `cmd/gateway/reconciler.go:44` — `ifaceName := "lab-" + lab.Name`. Это формат **VPN-ноги** (
  `LabIfaceName`), не gateway-ноги (`LabGWIfaceName = "gw-" + labName`). Gateway никогда не найдёт `lab-<n>` в своём
  netns (его там нет — только `gw-<n>`).
- **Что считать выполненным:** gateway использует `ovsnames.LabGWIfaceName(lab.Name)` (как node-agent);
  `assignGatewayIP` работает с правильным интерфейсом.
- **Заметки/gap:** сейчас reconciler уйдёт в вечный requeue `RequeueAfter: 5 * time.Second` потому что
  `LinkByName("lab-<n>")` всегда возвращает ENOTFOUND. **Gateway полностью нерабочий до фикса**.

#### REQ-GW-012: Gateway IP `10.9.N.1/24` на интерфейсе

- **Источник:** §2 «internet-диапазон: 10.{inet}.N.0/24, нога internet-gateway 10.{inet}.N.1».
- **Статус:** ✅ Implemented (логика правильная, имя интерфейса — нет, см. REQ-GW-011)
- **Реализация:** `routes.go:assignGatewayIP` — `nextIP(ipFromCIDR)` (`10.9.N.0` → `10.9.N.1`),
  `netlink.AddrAdd(link, ...)` с правильной маской; идемпотентен (EEXIST игнорируется).
- **Что считать выполненным:** после реконсайла в gateway-pod-е `ip addr show gw-<n>` показывает `10.9.N.1/24`.

---

### NAT (egress + inbound deny)

#### REQ-GW-020: SNAT/MASQUERADE для labCIDR через EXTERNAL_INTERFACE

- **Источник:** §9 «NAT в кластерную сеть → наружу».
- **Статус:** ✅ Implemented
- **Реализация:** `iptables.go:AddMasquerade` — `nat/POSTROUTING -s labCIDR -o extIface -j MASQUERADE` через
  `AppendUnique`.
- **Что считать выполненным:** трафик из `10.9.N.0/24` уходит наружу с source pod-а gateway; ответный трафик
  возвращается через conntrack.

#### REQ-GW-021: Cleanup SNAT при удалении Lab

- **Источник:** §14 (cleanup на DEL).
- **Статус:** ✅ Implemented
- **Реализация:** `reconcileDelete` → `IPT.DelMasquerade(cidr)` + `RemoveFinalizer("cybericebox.com/gateway")`.
- **Что считать выполненным:** удаление Lab → SNAT правило для её CIDR убирается; finalizer снят.

#### REQ-GW-022: Inbound из интернета запрещён

- **Источник:** §9 «обратные подключения из интернета запрещены», §13 «возврат — через NAT + conntrack».
- **Статус:** ⚠️ Partial (через conntrack, без явных DROP)
- **Реализация:** нет явных firewall-правил (FORWARD/INPUT DROP). Защита держится на:
    1. MASQUERADE без проброса портов — unsolicited inbound не имеет conntrack-state, не транслируется;
    2. cluster-уровне (Service не выставляет наружу);
    3. отсутствии DNAT.
- **Что считать выполненным:** unsolicited пакет с extIface к labCIDR падает; SYN извне не доходит до лабы; ответы на
  legitimate connections — проходят.
- **Заметки/gap:** §13 «возврат — через NAT + conntrack; различие подсетей per-лаба — условие, чтобы conntrack не путал
  сессии разных лаб» — это держится. Но было бы корректнее добавить явный FORWARD-policy DROP с
  `-m conntrack --ctstate ESTABLISHED,RELATED -j ACCEPT` как в VPN, чтобы не зависеть от глобального policy.

#### REQ-GW-023: Per-lab подсеть детерминирована из N

- **Источник:** §2 «различие подсетей per-лаба — условие, чтобы conntrack не путал сессии разных лаб».
- **Статус:** ✅ Implemented
- **Реализация:** `lab.Status.Internet.CIDR` имеет вид `10.9.N.0/24`, N разный для каждой Lab (заполняется
  оператором, [REQ-OP-021](./operator.md#req-op-021-per-lab-подсети-108n024-и-109n024-общий-n)).
- **Что считать выполненным:** conntrack возвращает пакет в правильную лабу: ответ от 8.8.8.8 на запрос из 10.9.5.X идёт
  обратно в лабу N=5, а не в лабу N=7.

---

### DHCP-сервер (§8)

#### REQ-GW-030: Реальный DHCPv4-сервер на лабовом интерфейсе

- **Источник:** §8 «Реальный DHCP-сервер (не эмуляция): сервер на шлюзе broadcast'ит по всему broadcast-домену, не
  завися от числа коммутаторов на пути».
- **Статус:** ⚠️ Partial
- **Реализация:** `dhcp.go:Start` использует `github.com/insomniacslk/dhcp/dhcpv4/server4.NewServer` на интерфейсе
  `gw-<n>` (UDP/67); один goroutine per-lab.
- **Что считать выполненным:** запросы DISCOVER/REQUEST от лабовых клиентов обрабатываются; клиент получает рабочий IP,
  gateway, DNS, lease 24h.

#### REQ-GW-031: YIAddr (offered IP) — назначение из диапазона

- **Источник:** §8 (полный набор настроек: подсеть/диапазон/gw/DNS).
- **Статус:** ❌ Missing
- **Реализация:** `dhcp.go:handler` — `dhcpv4.NewReplyFromRequest(msg, ...)` копирует поля из request, но **не
  задаёт `YIAddr`**. Опции вшиваются (Netmask, Router, DNS, LeaseTime), но фактический «выданный» IP остаётся 0.0.0.0.
- **Что считать выполненным:** в OFFER/ACK поле YIAddr содержит конкретный IP из `DHCPServer.Range` (или из labCIDR
  минус gateway minus reserved); разные клиенты получают разные IP; нет коллизий.
- **Заметки/gap:** **критическая поломка**. Клиент видит YIAddr=0.0.0.0 и сбрасывает ответ. DHCP не работоспособен.

#### REQ-GW-032: Range из spec.dhcpServer.range

- **Источник:** §8 «полный набор настроек (подсеть/диапазон/gw/DNS/опции)».
- **Статус:** ❌ Missing
- **Реализация:** `DHCPServer.Range` объявлено в типе
  CRD ([api/laboratory/v1alpha1/lab_types.go](../../api/laboratory/v1alpha1/lab_types.go)), но не читается в
  `reconciler.go` — поле `Range` игнорируется.
- **Что считать выполненным:** при `range="10.9.N.50,10.9.N.200"` сервер выдаёт IP только из этого диапазона;
  static-устройства (вне диапазона) не конфликтуют.

#### REQ-GW-033: Исключение static/none из выдачи

- **Источник:** §8 «Устройства со static/none исключаются из выдачи».
- **Статус:** ❌ Missing
- **Реализация:** нет — сервер не знает про static устройства лабы, у него нет доступа к Device CRD.
- **Что считать выполненным:** при DISCOVER от устройства с фиксированным IP сервер либо не отвечает (NACK), либо
  предлагает тот же IP (reservation по MAC).

#### REQ-GW-034: ≤1 DHCP-сервера на broadcast-домен

- **Источник:** §8 «≤1 DHCP-сервер на broadcast-домен — проверка по связной L2-компоненте».
- **Статус:** ❌ Missing
- **Реализация:** валидация графа лежит в обязанностях
  оператора ([REQ-OP-027](./operator.md#req-op-027-валидация-1-dhcp-сервера-на-broadcast-домен)) — её нет; gateway
  просто запускает DHCP по флагу.
- **Что считать выполненным:** оператор отвергает Lab с двумя DHCP-серверами в одной broadcast-компоненте; gateway
  получает запрос на старт DHCP только когда это безопасно.

#### REQ-GW-035: Lease 24 часа

- **Источник:** общепринятая практика (spec явно не задаёт).
- **Статус:** ✅ Implemented
- **Реализация:** `dhcp.go:handler` — `WithLeaseTime(86400)` для DISCOVER и REQUEST.
- **Что считать выполненным:** клиент обновляет lease раз в ~12 часов (T1=50% lease); не делает renew чаще нужного.

#### REQ-GW-036: Параметры из spec (Gateway, DNS, Subnet с fallback)

- **Источник:** §8 «полный набор настроек».
- **Статус:** ✅ Implemented
- **Реализация:** `reconciler.go:Reconcile` — `ds.Subnet ?? cidr`, `ds.Gateway ?? gwIP`, `ds.DNS ?? "8.8.8.8"`.
- **Что считать выполненным:** при пустом spec используются дефолты; при заданном — приоритет у spec.

#### REQ-GW-037: Lifecycle (start на create, stop на DEL)

- **Источник:** §14.
- **Статус:** ✅ Implemented
- **Реализация:** `DHCPManager.Start` (идемпотентен, no-op если уже запущен); `Stop` через `context.CancelFunc`. На
  DEL — `r.DHCP.Stop(lab.Name)`.
- **Что считать выполненным:** удаление Lab останавливает DHCP-goroutine; рестарт gateway pod-а пересоздаёт серверы.

#### REQ-GW-038: Lease persistence через рестарт

- **Источник:** общепринятая практика (spec не задаёт).
- **Статус:** ❌ Missing
- **Реализация:** нет; map в памяти.
- **Что считать выполненным:** при рестарте gateway-pod-а клиенты сохраняют свои IP (lease-database на диске /
  ConfigMap / Secret).
- **Заметки/gap:** для v1 допустимо отсутствие persistence (клиенты при потере связи реnewят).

---

### Lifecycle интеграции

#### REQ-GW-050: Wait `gw-<n>` interface перед start

- **Источник:** §14 (node-agent создаёт интерфейс асинхронно).
- **Статус:** ⚠️ Partial
- **Реализация:** `reconciler.go:Reconcile` — `LinkByName(ifaceName)` через `assignGatewayIP`; при ошибке
  `RequeueAfter: 5 * time.Second`.
- **Что считать выполненным:** интерфейс обнаружится за <5с после появления; pod gateway не пугается, что интерфейса ещё
  нет.
- **Заметки/gap:** имя интерфейса неверное (REQ-GW-011) → requeue вечный.

#### REQ-GW-051: Lab.Status.Internet.Ready после успешной настройки

- **Источник:** §10.
- **Статус:** ✅ Implemented
- **Реализация:** после `AddMasquerade` (+ опц. DHCP) → `lab.Status.Internet.Ready = true`, `Status().Update`.
- **Что считать выполненным:** kubectl видит Ready=true; оператор использует это для общего phase=Ready.

#### REQ-GW-052: Finalizer `cybericebox.com/gateway` — снят на DEL

- **Источник:** §14.
- **Статус:** ✅ Implemented
- **Реализация:** finalizer ставится
  оператором ([REQ-OP-030](./operator.md#req-op-030-finalizers-vpn-и-gateway-при-enabled)); снимается
  gateway-reconciler-ом в `reconcileDelete`.
- **Что считать выполненным:** Lab не удаляется пока gateway не снял SNAT и не остановил DHCP.

---

### Архитектурно: что Gateway НЕ делает

#### REQ-GW-060: Не L2-мостит лабы между собой

- **Источник:** §9 «egress-only».
- **Статус:** ✅ Implemented (по построению)
- **Реализация:** gateway имеет только NAT/POSTROUTING, не FORWARD-policy между лабами; пакет из лабы A не попадает в
  лабу B через gateway (он его MASQUERADE-ит наружу).
- **Что считать выполненным:** нет правил, разрешающих cross-lab forwarding.

#### REQ-GW-061: Не принимает входящие из интернета

- **Источник:** §9.
- **Статус:** ✅ Implemented (через отсутствие DNAT)
- **Реализация:** нет DNAT-правил, нет проброса портов наружу.
- **Что считать выполненным:** scan по public IP кластера к лабовым CIDR не находит лабовых сервисов.

#### REQ-GW-062: Не общий с VPN-сегментом

- **Источник:** §8 «VPN и internet в одном broadcast-домене — запрещено».
- **Статус:** ✅ Implemented (по построению + валидация в операторе)
- **Реализация:** gateway-нога `gw-<n>` отдельная от VPN-ноги `lab-<n>`; разные интерфейсы, разные подсети.
- **Что считать выполненным:** ARP-трафик из gateway-сегмента не виден в VPN-сегменте.

---

## Outstanding gaps

### Блокеры

- ❌ **REQ-GW-001** — оператор не разворачивает gateway-deployment в namespace группы. Pod просто не существует.
- ❌ **REQ-GW-011** — `reconciler.go:44` ищет `lab-<n>` вместо `gw-<n>`. Имя интерфейса не соответствует тому, что
  создаёт node-agent. **Gateway вечно requeue, ничего не работает**. Fix: использовать
  `ovsnames.LabGWIfaceName(lab.Name)`.
- ❌ **REQ-GW-031** — DHCP-сервер не назначает YIAddr (нет IP-пула / аллокации); клиенты получают 0.0.0.0. **DHCP по
  факту сломан**.

### Важно

- ❌ **REQ-GW-032** — `DHCPServer.Range` игнорируется.
- ❌ **REQ-GW-033** — static-устройства не исключаются из выдачи.
- ❌ **REQ-GW-034** — валидация ≤1 DHCP/домен лежит на операторе и тоже отсутствует.
- ⚠️ **REQ-GW-022** — inbound-deny держится на conntrack-state, не на явных firewall-rules. Стоит добавить
  FORWARD-policy.

### Желательно

- ❌ **REQ-GW-038** — lease-persistence через рестарт.

---

## Кросс-ссылки

- LabGroup → namespace → deployments (нужно добавить
  gateway): [operator.md REQ-OP-012](./operator.md#req-op-012-vpn-сервер-развёрнут-как-deployment1)
- OVS-нога `gw-<labname>` создаётся
  node-agent-ом: [node-agent.md REQ-NA-091](./node-agent.md#req-na-091-labifacereconciler--lab-name--gw-name-для-vpngateway-podов)
- Lab.Status.Internet.CIDR заполняется
  оператором: [operator.md REQ-OP-021](./operator.md#req-op-021-per-lab-подсети-108n024-и-109n024-общий-n)
- VPN-сервер — параллельная архитектура для входящего: [vpn.md](./vpn.md)
