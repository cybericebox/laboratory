# Proxy — спецификация требований (L7 + Demux)

> Источник: `cybericebox-spec-v1.md` §3 (challenge-токен; в коде Ed25519 / EdDSA), §4 (VPN-демукс: mac1, conntrack, XDP), §5 (L7
> challenge-прокси), §6 (TLS / wildcard DNS-01), §13 (изоляция).

## Назначение

Cluster-singleton-pod, который держит **два независимых L4/L7-демультиплексора на одном публичном IP**:

- **L7-прокси** (TCP/443) — HTTPS-фронт для web-заданий. Терминирует внешний TLS под wildcard-сертом
  `*.challenges.<domain>`, верифицирует собственную cookie сессии (HS256; выдаётся на `/_auth` по EdDSA-ссылке платформы), маршрутизирует в
  `<task>.labgroup-<groupID>.svc.cluster.local`, вырезает challenge-cookie перед форвардом.
- **WireGuard-демукс** (UDP/51820) — распределяет входящие WG-пакеты на VPN-сервер нужной группы. Handshake init (type
  1) через mac1 brute-force в userspace; transport data (type 4) через XDP-fast-path с conntrack-таблицей.

Оба демультиплексора смотрят в одни и те же LabGroup-CRD-ы (но разные поля): proxy.l7 — namespace `labgroup-<groupID>`;
demux — `LabGroup.Status.VPN.PublicKey`.

## Карта реализации

| Файл                                 | Роль                                                                                            |
|--------------------------------------|-------------------------------------------------------------------------------------------------|
| `cmd/proxy/main.go`                  | Bootstrap: cluster-singleton manager, watcher LabGroup, HTTPS-сервер + UDP-демукс + TTL-cleanup |
| `internal/proxy/config.go`           | env-config: TLS_CERT/KEY paths, BASE_DOMAIN, LISTEN_HTTPS, SESSION_SECRET, SESSION_COOKIE_NAME, REPORT_INTERVAL, UDP_LISTEN_ADDR |
| `cmd/proxy/l7/handler.go`            | ServeHTTP: parse host → validate cookie → resolve backend → strip cookie → reverse proxy        |
| `cmd/proxy/l7/auth.go`               | validateCookie: HS256 cookie прокси; verifyHandoff: EdDSA токен доступа, exp, host                       |
| `cmd/proxy/demux/demux.go`           | UDP-loop: dispatch type 1 (mac1), type 2 (learn), type 4 (userspace fallback)                   |
| `cmd/proxy/demux/table.go`           | in-memory pubkey→UID; mac1_key через BLAKE2s; LabGroupWatcher reconciler                        |
| `cmd/proxy/demux/conntrack.go`       | receiver_index → Socket; AddPartial/Complete/Lookup/UpdateRoaming; TTL-eviction                 |
| `cmd/proxy/demux/xdp/wg_demux.bpf.c` | eBPF program: parse Eth/IP/UDP/WG, lookup BPF map, rewrite L2/L3/L4 → XDP_TX                    |
| `cmd/proxy/demux/xdp/loader.go`      | bpf2go-generated loader + Update/Delete + cfg map (proxyIP, gw MAC)                             |

Статус-таксономия — см. [operator.md](./operator.md#условные-обозначения-статуса).

---

## Требования

### Cluster-singleton и watch

#### REQ-PX-001: Cluster-singleton (один pod на кластер)

- **Источник:** §4 «единый публичный вход; два слушателя на одном публичном IP».
- **Статус:** ✅ Implemented (по построению)
- **Реализация:** `main.go` — manager без `DefaultNamespaces` (cluster-wide watch); ожидается Deployment×1 в namespace
  `proxy-system` (NetworkPolicy в операторе ссылается на `proxy-system`/`app=proxy`).
- **Что считать выполненным:** ровно один pod `app=proxy` в namespace `proxy-system`; pod может работать на любой ноде,
  IP кластерный.

#### REQ-PX-002: Watch LabGroup (cluster-scoped) для маппинга pubkey↔UID

- **Источник:** §4 «Источник правды `server-pubkey → backend(podIP:port)` — в CRD/реестре».
- **Статус:** ✅ Implemented
- **Реализация:** `demux/table.go:LabGroupWatcher` — `For(&LabGroup{})`; на reconcile берёт `lg.Status.VPN.PublicKey`,
  вычисляет mac1_key, обновляет таблицу. На DEL — `Table.Delete(uid)`.
- **Что считать выполненным:** новая LabGroup появляется в demux-таблице за <5с; удалённая — пропадает.

#### REQ-PX-003: Watch Services для backend-resolution

- **Источник:** §5 «Разрешение = существование Service».
- **Статус:** ✅ Implemented
- **Реализация:** `main.go:svcResolver` — `mgr.GetClient().List(..., InNamespace("labgroup-<uid>"))`, ищет Service по
  имени task, читает `Spec.Ports[0].Name` как protocol.
- **Что считать выполненным:** Service создан оператором → proxy маршрутизирует; нет Service → HTTP 404.
- **Заметки:** через mgr.GetClient() задействован informer-cache (нет live-API call на каждый запрос).

#### REQ-PX-004: Cluster-wide RBAC

- **Источник:** REQ-PX-002 + REQ-PX-003.
- **Статус:** 🔍 Needs verification
- **Реализация:** в коде нет — RBAC через ServiceAccount/ClusterRole, не в репо.
- **Что считать выполненным:** в манифесте проекта (вне cmd/proxy) есть ClusterRole с verbs `get/list/watch` на
  `labgroups`, `services`; ClusterRoleBinding на ServiceAccount `proxy-system/proxy`.

---

### L7 Challenge Proxy (§5)

#### REQ-PX-010: HTTPS терминация на :443 (внешний всегда HTTPS)

- **Источник:** §5 «снаружи всегда HTTPS», §6 «терминация на L7-прокси».
- **Статус:** ✅ Implemented
- **Реализация:** `main.go` —
  `http.Server{Addr: cfg.ListenHTTPS, TLSConfig: {Certificates: [tlsCert], MinVersion: TLS1.2}}`,
  `httpsSrv.ListenAndServeTLS("", "")`.
- **Что считать выполненным:** только TLS-listener; нет plain HTTP; TLS 1.2+; cert загружен из конфига.

#### REQ-PX-011: Wildcard cert `*.challenges.<domain>` (ACME DNS-01)

- **Источник:** §6 «Wildcard-серты через ACME DNS-01... *.challenges.домен».
- **Статус:** 🔍 Needs verification
- **Реализация:** в proxy-коде серт **загружается из файла** (`tls.LoadX509KeyPair`); ACME-логика, DNS-01 — внешний
  процесс (cert-manager), не часть бинаря.
- **Что считать выполненным:** cert-manager (или эквивалент) в кластере выпускает wildcard через DNS-01; Secret
  монтируется в pod proxy.
- **Заметки/gap:** в репо нет конфигурации cert-manager. Уточнить инфраструктуру деплоя.

#### REQ-PX-012: Парсинг hostname → task

- **Источник:** §5 «task — из поддомена... строгая валидация charset/белый список».
- **Статус:** ✅ Implemented
- **Реализация:** `l7/handler.go:ServeHTTP` — strip port, check suffix `.<baseDomain>`, trim suffix → task; valid task
  regex `^[a-z0-9][a-z0-9\-]{0,62}$`. Невалидные — 400.
- **Что считать выполненным:** `foo.challenges.домен` → task=`foo`; `..` или `/` или `@` в task → 400; `<task>` ≤63
  chars.

#### REQ-PX-013: Anti-SSRF в task-validate

- **Источник:** §5 «task контролируется клиентом → строгая валидация charset/белый список, анти-SSRF».
- **Статус:** ✅ Implemented
- **Реализация:** regex отбрасывает path-traversal, dots, slashes; backend URL формируется через
  `fmt.Sprintf("%s://%s.%s.svc...", proto, task, ns, port)` — task попадает только в hostname-сегмент.
- **Что считать выполненным:** `task=`foo/../bar`` → 400 до обращения к resolver.

#### REQ-PX-014: Handoff-ссылка `/_auth` и собственная cookie прокси

- **Источник:** §3 (прокси держит только публичный ключ платформы) + решение владельца 2026-09-30: API и лабораторный
  домен разные, поэтому платформа cookie не ставит.
- **Статус:** ✅ Implemented (нужна проверка на живом кластере)
- **Реализация:** платформа по клику отдаёт ссылку `https://<device>-<code>.<base>/_auth?t=<jwt>`. JWT (токен доступа к лаборатории)
  подписан Ed25519 (alg `EdDSA`; RS256, HS256 и `none` отклоняются) ключом тенанта (`auth.go:verifyHandoff`: `iss` = тенант, `kid` = id ключа, `aud` = `laboratory-proxy`, `sub` = клиент, `nbf`; группа должна принадлежать `iss`), несёт
  `group_id`, `host` (метка `<device>-<code>`), `sess` (конец сессии), `iat`, `exp` (~1 минута, `LAB_ACCESS_TOKEN_TTL`
  на backend, не более 5). Токен без состояния: ни `jti`, ни привязки к браузеру, прокси ничего не запоминает. Прокси на
  `/_auth` (`session.go:handoff`) офлайн проверяет подпись, срок, что `host` совпадает с хостом запроса, и что сессия не
  закончилась. Затем ставит СВОЮ cookie (`Domain=<base>`, `HttpOnly`, `Secure`,
  `SameSite=Lax`, срок = `sess`), подписанную HMAC-SHA256 секретом прокси (`SESSION_SECRET`, не короче 32 байт, из Secret
  `proxy.l7.sessionSecret.name`, по умолчанию `proxy-session`; создаётся отдельно, не чартом), общий для реплик, платформе неизвестен, это не ключ доступа; имя cookie
  `SESSION_COOKIE_NAME`) и отвечает `303 /` с `Referrer-Policy: no-referrer`. `/_auth` в лабораторию не
  проксируется.
- **Обычные запросы:** `validateCookie` проверяет только HS256 cookie (токен доступа как cookie не принимается) и далее
  политику группы. Cookie нет, истекла или подпись неверна: без редиректа отдаётся статичная карточка (uk+en по
  `Accept-Language`, герб) «Сесія завершилася. Відкрийте лабораторію ще раз за посиланням із завдання.», статус 401.
- **Что считать выполненным:** `session_test.go` (успешный обмен, истёкшая ссылка, потолок 5 минут, чужой хост, подмена
  подписи, отказ RS256/HS256/`none`, карточка при отсутствующей и истёкшей cookie), `auth_test.go`, `config_test.go`
  (загрузка ключа Ed25519, отказ RSA, короткий `SESSION_SECRET`).

#### REQ-PX-015: Cookie claims

- **Источник:** §3 «Claims: { user_id, group_id, exp }» + решение владельца 2026-09-30 (учёт обращений по пользователям).
- **Статус:** ✅ Implemented
- **Реализация:** токен несёт только идентификаторы, известные оператору: `group_id` (LabGroup, единственный claim
  маршрутизации) и `client` (имя LabGroupClient этой группы, тот же объект, что и VPN-пир), плюс `host`, `iat` и `exp`
  (токен доступа живёт минуту, не более 5), `sess` (конец сессии: финиш события + 1 час; 24 часа для события без финиша;
  оператор сессию не продлевает). Идентификаторов платформы (событие, команда, пользователь) в токене нет.
  Выдаёт handoff-ссылки backend платформы для уже существующего клиента; сам токен в браузере не хранится.
- **Доступ:** режим один, по клиенту. Клиент обязателен, он должен существовать в группе токена (`LabGroupClient`), и
  лаборатория доступна, только если политика группы разрешает её этому клиенту (та же политика, что для VPN); нет
  политики или нет разрешающего правила: отказ. Блокировка клиента закрывает и VPN, и web сразу после синхронизации
  политики; валидный токен не отзывается — отказывает политика. Сессия без клиента: 401.
- **Учёт:** отчёты прокси ключуются `(группа, клиент, лаборатория)`, как отчёты VPN (`Subject` в `LabTrafficTouch` =
  имя клиента). Какие группы считаются событийными (`e-…-t-…`), решает backend; тестовые деплои и команда модераторов в
  аналитику не попадают, отдельного claim-маркера нет.
- **Тестовые деплои каталога:** группа `t-<uuid>`, клиент `p-<автор>`; токен живёт до конца аренды деплоя, платформа
  создаёт allow для клиента на лабораторию `lab` (`testdeploy_test.go`).
- **Что считать выполненным:** `handler_test.go` (клиент, авторизация), `policy_test.go`.

#### REQ-PX-019A: Учёт запросов по пользователям и лабораториям

- **Статус:** реализовано и проверено локальными API/RPC, TCP/WebSocket и kernel-прогонами; границы проверки указаны в отчёте 2026-10-07.
- Допущенный запрос регистрируется после проверки группы, клиента, Service, политики и квот. Его попытка и переданные байты видны во время выполнения, включая отмену. `firstResponded` отмечает настоящий ответ upstream; собственные ошибки прокси не считаются байтами лаборатории.
- HTTP считает байты тела. WebSocket считает переданный payload data/continuation frames в обе стороны, включая сжатый payload в переданном виде. Заголовки HTTP/TLS/TCP, заголовки/маски кадров и управляющие кадры исключены. Неподдерживаемая framing/extension-информация отмечает неполноту; содержимое сообщений не сохраняется.
- Ledger ключуется `(namespace группы, клиент, лаборатория)`. Сохранённые итоги этой реплики восстанавливаются один раз и продолжаются через перезапуск. Нехватка места или ошибка чтения baseline запрещает его перезапись; повторная подготовка безопасна. Лимиты строк и насыщение счётчиков сохраняют признаки неполноты.
- Каждый `LabTrafficReport/proxy-<pod>` содержит cumulative ledger и наблюдавшиеся `coverageSpans` с идентичностью writer/boot. Старые и новые интервалы не соединяются через простой. Scalar `coveredFrom/coveredTo` сохраняются для совместимости. Heartbeat выдаётся и выбранной лаборатории без строк, даже если другая лаборатория группы имеет трафик; чужие строки не раскрываются.
- Агент устраняет повторные writer-отчёты, безопасно складывает итоги и учитывает пропуски текущих Ready L7-реплик. Сохранённые kernel checkpoints и адрес пользователя в public protobuf не передаются. Наблюдения разных поверхностей (`vpn`, `proxy`) остаются различимыми.
- При остановке закрывается приём и активные HTTP/WebSocket-соединения, дожидаются завершения учёта и затем публикуются итоговые счётчики. Периодическая доставка не обещает восстановление ещё не опубликованных байтов после аварийного kill процесса.
- WireGuard demux передаёт зашифрованные datagrams и не собирает отдельную пользовательскую UDP-аналитику. VPN-сервер учитывает разрешённый трафик после расшифрования. HTTP/3 в этот контракт не входит.


#### REQ-PX-016: Cookie-вырезание перед форвардом

- **Источник:** §3 «Прокси вырезает challenge-cookie на входе — задание её не видит».
- **Статус:** ✅ Implemented
- **Реализация:** `handler.go:62-70` — клонирует request, фильтрует cookies, перезаписывает `Cookie` header без
  challenge-cookie.
- **Что считать выполненным:** в backend pod-е заголовок `Cookie:` не содержит `challenge=...`; остальные cookies
  сохранены.

#### REQ-PX-017: Backend URL `<task>.labgroup-<groupID>.svc`

- **Источник:** §5 «`<task>.<ns(group)>.svc`».
- **Статус:** ✅ Implemented
- **Реализация:** `l7/handler.go:ServiceResolver` —
  `fmt.Sprintf("%s://%s.%s.svc.cluster.local:%d", proto, task, "labgroup-"+groupID, port)`.
- **Что считать выполненным:** все запросы группы X идут только в её namespace `labgroup-X`; токен с подменённым
  `groupID` отвергается на стадии проверки подписи.

#### REQ-PX-018: Service existence as access gate

- **Источник:** §5 «Разрешение = существование Service: оператор создаёт Service для exposure.web. Нет web-доступа → нет
  Service → не резолвится → 404».
- **Статус:** ✅ Implemented
- **Реализация:** `main.go:svcResolver` возвращает ошибку при NotFound → handler → `http.Error(w, "not found", 404)`.
- **Что считать выполненным:** Lab без `exposure.web` → Service не существует → 404; никакой отдельной authz-проверки не
  нужно.

#### REQ-PX-019: Internal upstream — http (default) или https

- **Источник:** §5 «exposure.web.internal: http (дефолт) — внутрь plain HTTP; https — re-encrypt».
- **Статус:** ⚠️ Partial
- **Реализация:** `main.go:svcResolver` берёт `proto := svc.Spec.Ports[0].Name` (`"http"` или `"https"`), пустое →
  `"http"`; backend URL формируется с этим proto.
- **Что считать выполненным:** при `internal: https` proxy выполняет re-encrypt; self-signed cert инстанса принимается.
- **Заметки/gap:** `httputil.NewSingleHostReverseProxy(target)` без кастомного Transport использует
  `http.DefaultTransport`, который проверяет сертификат по системному CA-pool. Self-signed cert инстанса будет
  отвергнут. Нужен `InsecureSkipVerify: true` или custom CA. Сейчас re-encrypt в https-инстанс упадёт.

#### REQ-PX-020: X-Forwarded-Proto: https

- **Источник:** §5 «Прокси проставляет X-Forwarded-Proto: https».
- **Статус:** ✅ Implemented
- **Реализация:** `handler.go:71` — `r.Header.Set("X-Forwarded-Proto", "https")`.
- **Что считать выполненным:** backend pod видит правильный proto в заголовке; secure-cookies на стороне backend
  корректно ставятся.

#### REQ-PX-021: Внешний HTTP отсутствует

- **Источник:** §5 «Внешний HTTP отсутствует — он ломал бы Secure-cookie».
- **Статус:** ✅ Implemented (по построению)
- **Реализация:** в `main.go` создаётся только TLS-listener; нет `http.ListenAndServe(":80", ...)`.
- **Что считать выполненным:** `curl http://*.challenges.домен` не отвечает; redirect-механизма нет.

---

### WireGuard Demux (§4) — userspace часть

#### REQ-PX-030: UDP listener на :51820 (единый публичный порт)

- **Источник:** §4 «единый публичный порт».
- **Статус:** ✅ Implemented
- **Реализация:** `demux/demux.go:New` — `net.ListenUDP("udp4", :51820)`; конфигурируемо через `UDP_LISTEN_ADDR`.
- **Что считать выполненным:** все WG-handshakes/трафик стягиваются на один порт; параметр статичен.

#### REQ-PX-031: In-memory pubkey→UID таблица

- **Источник:** §4 «На горячем пути — in-memory таблица в демукс-сервисе, наполняется через watch. etcd на путь пакета
  не ставится».
- **Статус:** ✅ Implemented
- **Реализация:** `demux/table.go:Table` — `[]TableEntry{UID, Mac1Key}`, RW-mutex; LabGroupWatcher обновляет при
  изменении CRD.
- **Что считать выполненным:** lookup mac1 не делает API-call в k8s; etcd не задействован.

#### REQ-PX-032: mac1_key = BLAKE2s("mac1----" || pubkey)

- **Источник:** §4 «mac1_key[server] = HASH("mac1----" || serverPubKey)».
- **Статус:** ✅ Implemented
- **Реализация:** `table.go:computeMac1Key` — BLAKE2s-256, label `"mac1----"` (8 bytes), затем pubkey-bytes.
- **Что считать выполненным:** для тестового pubkey хеш совпадает с reference WireGuard implementation.

#### REQ-PX-033: Type 1 — brute-force mac1 lookup

- **Источник:** §4 «type 1 (handshake init) — на пакет перебор: проверить mac1 под каждым mac1_key».
- **Статус:** ✅ Implemented
- **Реализация:** `table.go:FindByMac1` — извлекает `mac1` из packet (bytes [len-32 : len-16]), вычисляет MAC для каждой
  записи с её ключом, сравнивает.
- **Что считать выполненным:** для 1000 серверов handshake-init обрабатывается за <1ms; нет API-call на путь пакета.

#### REQ-PX-034: Type 1 → forward на backend

- **Источник:** §4 (демукс пересылает на VPN-сервер группы).
- **Статус:** ⚠️ Partial
- **Реализация:** `demux.go:handleType1` — `net.ResolveUDPAddr` к `vpn.labgroup-<uid>.svc.cluster.local:51820`,
  `DialUDP` (эфемерный сокет), `Write(pkt)`, `Close()`. Conntrack `AddPartial(ci, src, server)`.
- **Что считать выполненным:** forward работает; type 2 response корректно получен (см. REQ-PX-035).
- **Заметки/gap:** **`serverConn.Close()`** сразу после `Write`. backend ответит type 2 на эфемерный socket, но он уже
  закрыт — ответ потеряется. (См. REQ-PX-035.)

#### REQ-PX-035: Type 2 — handshake response от backend к клиенту

- **Источник:** §4 «type 2 (handshake response) — исходящий от backend, на входе не маршрутизируется. На нём демукс
  выучивает связку».
- **Статус:** ❌ Missing (блокер)
- **Реализация:** `demux.go:handleType2` обрабатывает type 2 на основном listen-сокете, но backend (VPN-pod) отвечает на
  эфемерный сокет, открытый в `handleType1` и тут же закрытый. **Type 2 на основной сокет не попадает**.
- **Что считать выполненным:** backend инициирует "ответный поток" из своего pod-а к main-listen-сокету proxy ИЛИ proxy
  использует один stable сокет (с `SO_REUSEPORT` или просто listenUDP + `WriteToUDP` для отправки type 1, чтобы ответ
  пришёл туда).
- **Заметки/gap:** **handshake reply теряется** в текущей реализации. Правильный подход: отправлять type 1 через
  `d.conn.WriteToUDP(pkt, serverAddr)` — backend ответит на тот же `d.conn`, и type 2 прилетит в основной `Run()`-loop.

#### REQ-PX-036: Type 2 — learn conntrack

- **Источник:** §4 «На нём демукс выучивает связку», «`peer_index` в значении указывает на зеркальную запись».
- **Статус:** ⚠️ Partial (зависит от REQ-PX-035)
- **Реализация:** `handleType2` парсит Si, Ci → `conntrack.Lookup(ci)` для адреса клиента →
  `conn.WriteToUDP(pkt, clientDst)` → `conntrack.Complete(si, ci, clientSocket, serverSocket)`. Complete создаёт
  зеркальную запись `entries[si]` с `PeerIndex=ci` и обновляет `entries[ci].PeerIndex=si`. Sync в XDP-map.
- **Что считать выполненным:** после успешного handshake обе зеркальные записи существуют; type 4 может
  маршрутизироваться в обе стороны.
- **Заметки/gap:** алгоритм правильный, но из-за REQ-PX-035 type 2 фактически не доходит.

#### REQ-PX-037: Type 4 — userspace fallback

- **Источник:** §4 «type 4 (transport) — мaршрутизация по conntrack-таблице»; XDP — primary, userspace — fallback.
- **Статус:** ✅ Implemented
- **Реализация:** `demux.go:handleType4Userspace` — `binary.LittleEndian.Uint32(pkt[4:8])` (receiver_index),
  `conntrack.Lookup` → forward через `d.conn.WriteToUDP`.
- **Что считать выполненным:** при недоступности XDP type 4 продолжает работать в userspace; `last_seen` обновляется
  через Lookup.

#### REQ-PX-038: Collision check при handshake

- **Источник:** §4 «Коллизия индексов между группами возможна... гасится на хендшейке: при записи type 1/type 2, если
  индекс уже принадлежит другой живой сессии → дроп».
- **Статус:** ❌ Missing
- **Реализация:** `conntrack.go:AddPartial` и `Complete` просто перезаписывают `entries[ci]` / `entries[si]` без
  проверки коллизии.
- **Что считать выполненным:** при `AddPartial(ci=X, ...)` если `entries[X]` уже занят другой живой (`last_seen < TTL`)
  сессией → пакет дропается; апстрим WG повторит handshake с новым индексом.
- **Заметки/gap:** без этой проверки две группы с одинаковым случайным `Ci` будут перетерать друг друга → одна сессия
  молча сломается.

#### REQ-PX-039: Roaming detection в userspace type 4

- **Источник:** §4 «src пакета ≠ sender_socket → форвардим (индекс уникален) и обновляем сокет».
- **Статус:** ❌ Missing
- **Реализация:** `handleType4Userspace` делает только Lookup+forward; не сравнивает `src` с `entry.SenderSocket`;
  `UpdateRoaming` определён в conntrack.go, но **не вызывается**.
- **Что считать выполненным:** при изменении клиентского endpoint (WiFi → LTE) userspace тоже обновляет sender_socket в
  conntrack-зеркале (как XDP в `wg_demux.bpf.c`).

#### REQ-PX-040: TTL eviction (~3 мин по бездействию)

- **Источник:** §4 «TTL по бездействию (~3 мин)».
- **Статус:** ✅ Implemented
- **Реализация:** `conntrack.go:RunTTLCleanup` — тикер `TTL/3 = 1 min`, `time.Since(LastSeen) > TTL`; `evictEntry`
  удаляет обе зеркальные записи; XDP map чистится через `xdp.Delete`.
- **Что считать выполненным:** клиент молчит >3 мин → его записи (Ci, Si) удалены; следующий пакет → новый handshake.

#### REQ-PX-041: last_seen обновляется на type 4

- **Источник:** §4 «`last_seen` обновляется на каждом type 4 (в XDP)».
- **Статус:** ⚠️ Partial
- **Реализация:** userspace — `Lookup` обновляет `e.LastSeen = time.Now()` ✅. XDP — нужно проверить в `wg_demux.bpf.c` (
  обычно делается в Go-side update; в текущем BPF map это просто HASH без timestamp).
- **Что считать выполненным:** при load XDP-программа также обновляет `last_seen` в значении (или другой стратегией:
  периодическая синхронизация из BPF в userspace).
- **Заметки/gap:** в BPF-структуре `WgDemuxDstEntry` нет поля `last_seen`. Запись считается «живой» пока существует в
  map. TTL-эвикция базируется на userspace counter, который НЕ обновляется на XDP fast-path → запись будет evicted даже
  при активной XDP-сессии.

---

### WireGuard Demux (§4) — XDP fast-path

#### REQ-PX-050: XDP-программа загружается на public interface

- **Источник:** §4 «XDP: type 4 (lookup + форвард + обновление сокета + last_seen)».
- **Статус:** ✅ Implemented (best-effort)
- **Реализация:** `demux.go:New` — `xdp.Load("eth0", wgPort)`; failure non-fatal (логируется, userspace fallback).
- **Что считать выполненным:** на загрузочной ноде на интерфейсе `eth0` подключена XDP-программа; `ip link show eth0`
  показывает `xdp/xdp` в attached.
- **Заметки:** `eth0` захардкожен; если public interface другой — параметризовать.

#### REQ-PX-051: XDP — forward type 4 через rewrite L2/L3/L4

- **Источник:** §4 (XDP: lookup + forward).
- **Статус:** ✅ Implemented
- **Реализация:** `xdp/wg_demux.bpf.c` — parse Eth+IPv4+UDP+WG, extract `receiver_index` (bytes 4-7 LE), lookup в
  `wg_sessions` map, rewrite eth.dst (gw MAC), ip.src (proxy IP), ip.dst (target), checksum, udp.src (proxy port),
  udp.dst (target port), `XDP_TX`.
- **Что считать выполненным:** type 4-пакет с известным receiver_index уходит на NIC за один XDP-проход; type 1/2 →
  `XDP_PASS` в userspace.

#### REQ-PX-052: XDP cfg map (proxy IP, gateway MAC)

- **Источник:** §4 (XDP rewrite требует знать proxy address).
- **Статус:** ✅ Implemented
- **Реализация:** `xdp/loader.go:Load` — `ifaceIPv4(iface)` + `gatewayMAC()` (ARP lookup default route) → write в
  `xdp_cfg_map[0]`.
- **Что считать выполненным:** после Load в BPF map ключ 0 содержит правильные proxy IP/port + gateway MAC; ARP-кеш на
  host-е разогрет (иначе MAC=0).

#### REQ-PX-053: XDP map update/delete с userspace

- **Источник:** §4 (mirrored writes в Complete и evictEntry).
- **Статус:** ✅ Implemented
- **Реализация:** `conntrack.go:Complete` → `xdp.Update(ci, server)`, `xdp.Update(si, client)`; `evictEntry` →
  `xdp.Delete(idx)`, `xdp.Delete(peer)`.
- **Что считать выполненным:** после handshake type 4-пакеты обеих сторон обрабатываются XDP; после TTL — пакет с тем же
  receiver_index получает XDP_PASS → userspace дропает (нет записи).

#### REQ-PX-054: XDP роуминг — `peer_index` для зеркальной правки

- **Источник:** §4 «через peer_index находим зеркальную запись и правим в ней receiver_socket. Делает сам XDP».
- **Статус:** 🔍 Needs verification
- **Реализация:** в `wg_demux.bpf.c` (читал ранее): обновление при роуминге обычно требует чтения отдельной map
  `peer_index_map`. Сейчас map содержит только `{IP, Port}` без peer_index — обновление зеркала из XDP невозможно.
- **Что считать выполненным:** при изменении src в type 4 XDP обновляет зеркальную запись (или хотя бы свою) без дропа.
- **Заметки/gap:** BPF-карта не несёт `peer_index`, как требует §4. Роуминг fast-path не работает.

---

### Challenge token (§3)

#### REQ-PX-060: Stateless — нет БД на прокси

- **Источник:** §3 «Stateless, асимметричная подпись».
- **Статус:** ✅ Implemented
- **Реализация:** validateCookie не делает API-call/DB-call; всё в самой подписи (cookie прокси). Состояния нет
  совсем: ни кэша `jti`, ни привязки к браузеру.
- **Что считать выполненным:** в proxy-pod-е нет Redis/DB-соединений.

#### REQ-PX-061: Только public key (не приватник)

- **Источник:** §3 «прокси держит только публичный ключ: проверять может, ковать — нет».
- **Статус:** ✅ Implemented
- **Реализация:** публичные ключи доступа тенантов (PKIX PEM Ed25519) лежат в Secret `tenant-<имя>-access-keys` namespace `laboratory-tenants`
  (по записи на id ключа; пишет агент, прокси их только читает, `l7.SecretKeys`); общего ключа нет; private-key нет в env / Secret / mount.
- **Что считать выполненным:** компрометация proxy не даёт ковать challenge-токены.

#### REQ-PX-062: Cookie scope `Domain=<base>`

- **Источник:** §3 «Domain=challenges.домен, HttpOnly + Secure + SameSite=Lax».
- **Статус:** ✅ Implemented
- **Реализация:** cookie ставит сам прокси на `/_auth` (`session.go`), домен `BASE_DOMAIN`, поэтому она уходит на все
  `<device>-<code>.<base>` и больше никуда. Платформа cookie не ставит и домена лабораторий не знает.

---

### TLS (§6)

#### REQ-PX-070: Wildcard cert загружен из файла

- **Источник:** §6.
- **Статус:** ✅ Implemented
- **Реализация:** `main.go` — `tls.LoadX509KeyPair(cfg.TLSCertPath, cfg.TLSKeyPath)`.
- **Что считать выполненным:** при ротации серта pod перезапускается (или нужен hot-reload — отложено).

#### REQ-PX-071: Hot-reload серта при ротации cert-manager

- **Источник:** общая ops-практика.
- **Статус:** ❌ Missing
- **Реализация:** нет watcher на cert-файл; перезагрузка только через рестарт.
- **Что считать выполненным:** при обновлении Secret серта (cert-manager renew) proxy подхватывает без рестарта (как в
  `cmd/main.go` operator-а через `certwatcher.New`).

---

### Изоляция между демультиплексорами

#### REQ-PX-080: VPN и L7 — независимые слои

- **Источник:** §1, §4 «два слушателя на одном публичном IP — разные демультиплексоры на разных слоях».
- **Статус:** ✅ Implemented
- **Реализация:** `demux/` и `l7/` — отдельные пакеты; общая только метадата (LabGroupWatcher для demux, ServiceList для
  l7). Нет shared state между ними кроме mgr.GetClient.
- **Что считать выполненным:** ошибка в L7 (паника на запросе) не ломает UDP-демукс; и наоборот.

#### REQ-PX-081: VPN-стороне нет дешифровки identity клиента

- **Источник:** §4 «Демукс по клиентскому pubkey невозможен — identity клиента в initiation зашифрована; расшифровка
  требовала бы приватник сервера на входе (отвергнуто)».
- **Статус:** ✅ Implemented (по построению)
- **Реализация:** код демукса не дешифрует initiation; маршрутизация по mac1 (computed from server pubkey, public
  knowledge).
- **Что считать выполненным:** на proxy-pod-е нет приватных ключей VPN-серверов; только публичные.

---

## Outstanding gaps

### Блокеры

- ❌ **REQ-PX-035** — handshake response (type 2) теряется: backend отвечает на эфемерный сокет, который сразу закрыт. *
  *WireGuard handshake невозможен через демукс**. Fix: использовать `d.conn.WriteToUDP(pkt, serverAddr)` вместо
  отдельного `DialUDP` (тогда ответ придёт на main listen-socket).
- ❌ **REQ-PX-038** — нет collision-check на handshake между группами с одинаковым случайным index.
- ❌ **REQ-PX-039** — roaming detection в userspace type 4 отсутствует (`UpdateRoaming` определён, не вызывается).
- ⚠️ **REQ-PX-019** — re-encrypt в https-инстанс упадёт из-за TLS-verify на self-signed серт. Нужен custom Transport с
  `InsecureSkipVerify` или CA-pool.

### Важно

- 🔍 **REQ-PX-054** — XDP роуминг (через peer_index) не поддерживается в BPF map (нет поля `peer_index`).
- ⚠️ **REQ-PX-041** — last_seen в XDP не обновляется → активные XDP-сессии могут быть evicted TTL-cleanup-ом.
- 🔍 **REQ-PX-004** — RBAC ClusterRole / ServiceAccount для proxy не виден в репо.
- 🔍 **REQ-PX-011** — ACME DNS-01 / cert-manager инфраструктура — внешняя.

### Желательно

- ❌ **REQ-PX-071** — hot-reload TLS-серта.

---

## Кросс-ссылки

- LabGroup pubkey источник (для demux table): [operator.md REQ-OP-011..013](./operator.md#labgroup-reconciler)
- Service создаётся оператором для
  web-exposure: [operator.md REQ-OP-028](./operator.md#req-op-028-web-exposure--service--networkpolicy)
- VPN-сервер группы — куда демукс шлёт type 1 (
  `vpn.labgroup-<UID>.svc`): [vpn.md REQ-VPN-010](./vpn.md#req-vpn-010-кластерная-нога-pod-а-для-демукс-форварда)
- NetworkPolicy на web-устройстве — ingress только от
  `proxy-system/app=proxy`: [operator.md REQ-OP-028](./operator.md#req-op-028-web-exposure--service--networkpolicy)
