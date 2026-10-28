//go:build ignore

// The in-kernel fast path of the WireGuard demux. It forwards transport packets (type 4) of sessions the demux has put into
// wg_sessions and leaves everything else to the demux socket (the slow path): handshakes, unknown indices, stale sessions,
// strange or roamed sources. Both address pairs are rewritten exactly as the slow path would write them; the WireGuard
// payload never changes.
//
//   wg_tc_ingress       tcx ingress, external device: client -> server, and frames the XDP program already rewrote
//   wg_tc_reply         tcx ingress, cilium_host and the host-side veth of same-node servers: server -> client
//   wg_tc_egress_guard  tcx egress, external device: lets our replies skip Cilium's NodePort NAT
//   wg_xdp              xdp, external device: client -> server rewrite, handed to wg_tc_ingress through XDP metadata
//
// Bandwidth: besides the packet bucket every session has a byte bucket (rate and burst from wg_cfg, set by the demux to the session's
// fair share of the entry class), and all sessions together draw from one class window (wg_class): at most class_window_bytes in
// each window of class_window_ns. Both are off while their numbers are zero, and the whole byte limiter compiles away when the demux
// loads the program with byte_limiter = 0 (the fallback when the verifier of a kernel refuses it: the packet bucket stays).
//
// See the decision section "Datapath decisions (Task 4 spike)" of the demux modes plan for why packets move this way.

#include <linux/bpf.h>
#include <linux/if_ether.h>
#include <linux/ip.h>
#include <linux/in.h>
#include <linux/udp.h>
#include <bpf/bpf_helpers.h>
#include <bpf/bpf_endian.h>

#ifndef AF_INET
#define AF_INET 2
#endif
#ifndef IP_MF
#define IP_MF 0x2000
#endif
#ifndef IP_OFFSET
#define IP_OFFSET 0x1FFF
#endif

#define WG_HANDSHAKE_INIT 1     // message type and the three reserved zero bytes, read little endian
#define WG_TRANSPORT 4
#define WG_HANDSHAKE_INIT_LEN 148
#define WG_TRANSPORT_MIN_LEN 32 // header 16 + an empty payload's tag 16

#define WG_F_TO_CLIENT 1        // wg_fwd.flags: the server -> client direction

// Cilium reads the 0x0f00 nibble of skb->mark as its magic. 0x0c00 (MARK_MAGIC_HOST) is what its own iptables rule puts on
// every host-originated packet, so a request carrying it enters Cilium like one the demux socket sent (identity host).
#define CILIUM_MAGIC_MASK 0x0f00
#define CILIUM_MAGIC_HOST 0x0c00

// The mark of our replies, seen only by wg_tc_egress_guard. Its low 16 bits are zero, so it holds none of Cilium's magic
// values (all of them live in the 0x0f00 nibble, and Cilium reads the high half as an identity only together with one of them)
// and none of kube-proxy's 0x4000/0x8000; the high half spells "WG". Between the reply hook and the guard the packet passes no
// Cilium program, and the guard also checks the source tuple, so a foreign packet with the same value still goes to Cilium.
#define WG_REPLY_MARK 0x57470000

// XDP metadata: the frame was rewritten by wg_xdp; the original tuple and UDP checksum let tc fix the checksum with the
// skb helpers (which know the skb's checksum state) or put the frame back when it cannot be forwarded.
#define WG_META_MAGIC 0x57474d31 // "WGM1"

#define PKT_LEN (sizeof(struct ethhdr) + sizeof(struct iphdr) + sizeof(struct udphdr) + 8)
#define UDP_CSUM_OFF (sizeof(struct ethhdr) + sizeof(struct iphdr) + offsetof(struct udphdr, check))

// Set by the loader before the program is loaded (a read-only constant, so the verifier drops the byte limiter's code when it is 0).
volatile const __u32 byte_limiter = 1;

// The refill of the token bucket counts at most this much idle time at once (keeps elapsed * rate within 64 bits).
#define MAX_REFILL_NS 10000000000ULL

struct wg_key {
	__u32 index; // receiver index, host order as read little endian from the packet
};

struct wg_addr {
	__be32 ip;
	__be16 port;
	__u16 pad;
};

// One direction of a session, keyed by the receiver index its packets carry. The demux writes both directions of a session:
//
//   - session is the same in both: the receiver index of the client -> server direction. That entry (the owner, where
//     session equals its own index) holds the session's one token bucket and last_seen, as the slow path keeps one bucket and
//     one seen per session; the server -> client entry draws from the owner's. Without its owner a direction is a miss.
//   - exp_* is the only source accepted; new_src_* and dst_* are written into the packet.
//   - server -> client (WG_F_TO_CLIENT) also needs reply_ifindex, the device the server's replies arrive on (0 = replies stay
//     on the slow path), and via_*, the address the server sends to (the demux socket's source towards it, the client ->
//     server direction's new_src); a reply to any other address is not this session's. A reply entry is tied to its owner by
//     these tuples too: exp_* equals the owner's dst_* and via_* the owner's new_src_*, else the reply is a miss.
//   - The owner's last_seen_ns starts at the time of the write (CLOCK_MONOTONIC); its bucket fields start at zero (the first
//     packet fills the bucket). Writing the owner again starts a new bucket. In the other direction these fields are unused.
struct wg_fwd {
	__be32 exp_ip, new_src_ip, dst_ip;
	__be16 exp_port, new_src_port, dst_port, flags;
	__u32 session;
	__u32 reply_ifindex;
	__be32 via_ip;
	__be16 via_port, pad;
	__u32 pad2;
	__u64 last_seen_ns;
	struct bpf_spin_lock lock;
	__u32 pad3;
	__u64 tokens_x1000, refill_ns;
	__u64 byte_tokens, byte_refill_ns; // the byte bucket, same rules; bytes, not thousandths
};

struct wg_cfg {
	__u64 ttl_ns, rate_per_s, burst; // rate 0 = unlimited
	__u32 ext_ifindex;               // the external device replies leave through
	__u32 pad;
	__u64 byte_rate, byte_burst;                 // per session, both directions together, bytes per second and bytes; rate 0 = unlimited
	__u64 class_window_ns, class_window_bytes;   // the node's entry class: at most this many bytes in a window; bytes 0 = unlimited
};

// The class window: the bytes taken in the window that began at window_start_ns. Updated without a lock (atomic adds): a reset
// that two CPUs race to do costs at most one window of slack.
struct wg_class {
	__u64 window_start_ns;
	__u64 used;
};

struct wg_meta {
	__u32 magic;
	__be32 orig_saddr, orig_daddr;
	__be16 orig_sport, orig_dport, orig_check, pad;
};

enum wg_stat {
	WG_STAT_FORWARDED_TO_SERVER,
	WG_STAT_FORWARDED_TO_CLIENT,
	WG_STAT_PASSED_MISS,
	WG_STAT_PASSED_SOURCE,
	WG_STAT_PASSED_STALE,
	WG_STAT_DROPPED_RATE,
	WG_STAT_PASSED_ROUTE, // matched, but no route or neighbour for the rewritten packet: left to the slow path
	WG_STAT_PASSED_META,  // matched in XDP, but the driver gave no room for metadata: tc handles it as in tc mode
	WG_STAT_MAX,
};

struct {
	__uint(type, BPF_MAP_TYPE_HASH);
	__uint(max_entries, 100000);
	__type(key, struct wg_key);
	__type(value, struct wg_fwd);
	__uint(pinning, LIBBPF_PIN_BY_NAME);
} wg_sessions SEC(".maps");

struct {
	__uint(type, BPF_MAP_TYPE_HASH);
	__uint(max_entries, 64);
	__type(key, struct wg_addr);
	__type(value, __u8);
	__uint(pinning, LIBBPF_PIN_BY_NAME);
} wg_entries SEC(".maps");

struct {
	__uint(type, BPF_MAP_TYPE_LRU_HASH);
	__uint(max_entries, 65536);
	__type(key, struct wg_addr);   // client
	__type(value, struct wg_addr); // the entry address its handshake initiation arrived at
	__uint(pinning, LIBBPF_PIN_BY_NAME);
} wg_entry_seen SEC(".maps");

struct {
	__uint(type, BPF_MAP_TYPE_ARRAY);
	__uint(max_entries, 1);
	__type(key, __u32);
	__type(value, struct wg_cfg);
	__uint(pinning, LIBBPF_PIN_BY_NAME);
} wg_cfg SEC(".maps");

struct {
	__uint(type, BPF_MAP_TYPE_ARRAY);
	__uint(max_entries, 1);
	__type(key, __u32);
	__type(value, struct wg_class);
	__uint(pinning, LIBBPF_PIN_BY_NAME);
} wg_class SEC(".maps");

struct {
	__uint(type, BPF_MAP_TYPE_PERCPU_ARRAY);
	__uint(max_entries, WG_STAT_MAX);
	__type(key, enum wg_stat);
	__type(value, __u64);
	__uint(pinning, LIBBPF_PIN_BY_NAME);
} wg_stats SEC(".maps");

struct pkt {
	struct ethhdr *eth;
	struct iphdr *ip;
	struct udphdr *udp;
	__u32 type;  // WireGuard message type with the reserved bytes
	__u32 index; // receiver index (transport) / sender index (initiation): bytes 4..7
	__u32 len;   // UDP payload length
	__u32 bytes; // the packet's real length without the Ethernet header: what the byte limiters charge
};

struct tuple {
	__be32 saddr, daddr;
	__be16 sport, dport;
};

enum judgement { J_FORWARD, J_MISS, J_SOURCE, J_STALE, J_RATE };

static __always_inline void count(enum wg_stat s)
{
	__u64 *v = bpf_map_lookup_elem(&wg_stats, &s);
	if (v)
		*v += 1;
}

static __always_inline void count_judgement(enum judgement j)
{
	switch (j) {
	case J_MISS:
		count(WG_STAT_PASSED_MISS);
		break;
	case J_SOURCE:
		count(WG_STAT_PASSED_SOURCE);
		break;
	case J_STALE:
		count(WG_STAT_PASSED_STALE);
		break;
	case J_RATE:
		count(WG_STAT_DROPPED_RATE);
		break;
	default:
		break;
	}
}

// parse accepts Ethernet + IPv4 without options, not a fragment, + UDP with at least the 8 bytes of a WireGuard header. real_len is
// what the device really delivered after the Ethernet header (skb->len, or the frame in XDP): the lengths in the headers are the
// sender's word, so a packet whose IP or UDP length is not the real length (trailing bytes, a short header) is not parsed and
// goes to the slow path, and what the limiters charge is the real length, never the claimed one.
static __always_inline int parse(void *data, void *data_end, __u32 real_len, struct pkt *p)
{
	struct ethhdr *eth = data;
	struct iphdr *ip = (void *)(eth + 1);
	struct udphdr *udp = (void *)(ip + 1);
	__u8 *wg = (void *)(udp + 1);

	if ((void *)(wg + 8) > data_end)
		return -1;
	if (eth->h_proto != bpf_htons(ETH_P_IP))
		return -1;
	if (ip->version != 4 || ip->ihl != 5 || ip->protocol != IPPROTO_UDP)
		return -1;
	if (ip->frag_off & bpf_htons(IP_MF | IP_OFFSET))
		return -1;
	__u32 ulen = bpf_ntohs(udp->len);
	if (bpf_ntohs(ip->tot_len) != real_len || ulen < sizeof(*udp) + 8 || ulen + sizeof(*ip) != real_len)
		return -1;

	p->eth = eth;
	p->ip = ip;
	p->udp = udp;
	p->type = (__u32)wg[0] | ((__u32)wg[1] << 8) | ((__u32)wg[2] << 16) | ((__u32)wg[3] << 24);
	p->index = (__u32)wg[4] | ((__u32)wg[5] << 8) | ((__u32)wg[6] << 16) | ((__u32)wg[7] << 24);
	p->len = ulen - sizeof(*udp);
	p->bytes = real_len;
	return 0;
}

// frame_len is the length of an XDP frame after the Ethernet header.
static __always_inline __u32 frame_len(struct xdp_md *ctx)
{
	return (__u32)((long)ctx->data_end - (long)ctx->data) - sizeof(struct ethhdr);
}

static __always_inline int is_entry(__be32 ip, __be16 port)
{
	struct wg_addr a = {.ip = ip, .port = port};
	return bpf_map_lookup_elem(&wg_entries, &a) != NULL;
}

// record_entry remembers which entry address a client's handshake initiation arrived at: the source of the replies towards
// that client (ruling 5).
static __always_inline void record_entry(struct pkt *p)
{
	if (p->type != WG_HANDSHAKE_INIT || p->len != WG_HANDSHAKE_INIT_LEN || !is_entry(p->ip->daddr, p->udp->dest))
		return;
	struct wg_addr client = {.ip = p->ip->saddr, .port = p->udp->source};
	struct wg_addr entry = {.ip = p->ip->daddr, .port = p->udp->dest};
	bpf_map_update_elem(&wg_entry_seen, &client, &entry, BPF_ANY);
}

// take refills the session's buckets and takes one packet and len bytes from them (the userspace bucket of conntrack.go, in
// thousandths of a token for the packets), and stamps last_seen when the packet may go. A packet that does not fit one of the buckets
// takes nothing from the other.
static __always_inline int take(struct wg_fwd *f, struct wg_cfg *cfg, __u64 len, __u64 now)
{
	__u64 rate = cfg->rate_per_s;
	__u64 full = cfg->burst * 1000;
	int ok = 1;

	bpf_spin_lock(&f->lock);
	if (rate) {
		if (!f->refill_ns) {
			f->tokens_x1000 = full;
			f->refill_ns = now;
		}
		if (now > f->refill_ns) {
			__u64 elapsed = now - f->refill_ns;
			if (elapsed > MAX_REFILL_NS)
				elapsed = MAX_REFILL_NS;
			__u64 add = elapsed * rate / 1000000;
			f->tokens_x1000 += add;
			if (f->tokens_x1000 >= full) {
				f->tokens_x1000 = full;
				f->refill_ns = now;
			} else {
				// Advance only by the time that was turned into tokens, so the remainder is not lost.
				f->refill_ns += add * 1000000 / rate;
			}
		}
		if (f->tokens_x1000 < 1000)
			ok = 0;
	}
	if (byte_limiter && cfg->byte_rate) {
		__u64 brate = cfg->byte_rate;
		if (!f->byte_refill_ns) {
			f->byte_tokens = cfg->byte_burst;
			f->byte_refill_ns = now;
		}
		if (now > f->byte_refill_ns) {
			__u64 elapsed = now - f->byte_refill_ns;
			if (elapsed > MAX_REFILL_NS)
				elapsed = MAX_REFILL_NS;
			__u64 add = elapsed * brate / 1000000000ULL;
			f->byte_tokens += add;
			if (f->byte_tokens >= cfg->byte_burst) {
				f->byte_tokens = cfg->byte_burst;
				f->byte_refill_ns = now;
			} else {
				f->byte_refill_ns += add * 1000000000ULL / brate;
			}
		}
		if (f->byte_tokens < len)
			ok = 0;
	}
	if (ok) {
		if (rate)
			f->tokens_x1000 -= 1000;
		if (byte_limiter && cfg->byte_rate)
			f->byte_tokens -= len;
		f->last_seen_ns = now;
	}
	bpf_spin_unlock(&f->lock);
	return ok;
}

// class_take counts len bytes in the node's class window and says whether they fit (the bytes of a packet that does not fit are
// given back, so a refused packet does not eat into the window).
static __always_inline int class_take(struct wg_cfg *cfg, __u64 len, __u64 now)
{
	if (!byte_limiter || !cfg->class_window_bytes)
		return 1;
	__u32 zero = 0;
	struct wg_class *c = bpf_map_lookup_elem(&wg_class, &zero);
	if (!c)
		return 1;
	if (now < c->window_start_ns || now - c->window_start_ns >= cfg->class_window_ns) {
		c->used = 0;
		c->window_start_ns = now;
	}
	__sync_fetch_and_add(&c->used, len);
	if (c->used > cfg->class_window_bytes) {
		__sync_fetch_and_add(&c->used, (__u64)0 - len);
		return 0;
	}
	return 1;
}

// judge is the slow path's LookupForward in the kernel: a known index of this direction, fresh, from exactly the expected
// source (a roam goes to the slow path), within the session's rate. ifindex is the arrival device (replies only). The
// session's bucket and last_seen are the owner's (see struct wg_fwd), so both directions draw from one bucket.
static __always_inline enum judgement judge(struct pkt *p, int to_client, __u32 ifindex, struct wg_fwd **out,
					    struct wg_cfg **cfg_out)
{
	__u32 zero = 0;
	struct wg_cfg *cfg = bpf_map_lookup_elem(&wg_cfg, &zero);
	if (!cfg)
		return J_MISS;
	struct wg_key key = {.index = p->index};
	struct wg_fwd *f = bpf_map_lookup_elem(&wg_sessions, &key);
	if (!f || !(f->flags & WG_F_TO_CLIENT) != !to_client)
		return J_MISS;
	struct wg_fwd *owner = f;
	if (to_client) {
		if (!f->reply_ifindex || !f->via_ip)
			return J_MISS;
		// A reply goes to the demux socket's address only; the server's other traffic is not this session's.
		if (p->ip->daddr != f->via_ip || p->udp->dest != f->via_port)
			return J_MISS;
		if (f->reply_ifindex != ifindex)
			return J_SOURCE;
		struct wg_key okey = {.index = f->session};
		owner = bpf_map_lookup_elem(&wg_sessions, &okey);
		// The owner must be this session's other direction (the reply comes from where the owner sends to, and goes to
		// where the owner sends from), not another session that reuses the index: never charge or refresh a stranger.
		if (!owner || (owner->flags & WG_F_TO_CLIENT) || owner->session != f->session ||
		    owner->dst_ip != f->exp_ip || owner->dst_port != f->exp_port ||
		    owner->new_src_ip != f->via_ip || owner->new_src_port != f->via_port)
			return J_MISS;
	} else if (f->session != p->index) {
		return J_MISS;
	}
	__u64 now = bpf_ktime_get_ns();
	__u64 seen = owner->last_seen_ns;
	if (now > seen && now - seen > cfg->ttl_ns)
		return J_STALE;
	if (p->ip->saddr != f->exp_ip || p->udp->source != f->exp_port)
		return J_SOURCE;
	__u64 len = p->bytes;
	if (!take(owner, cfg, len, now))
		return J_RATE;
	if (!class_take(cfg, len, now))
		return J_RATE;
	*out = f;
	*cfg_out = cfg;
	return J_FORWARD;
}

static __always_inline __u16 csum_fold(__u32 sum)
{
	sum = (sum & 0xffff) + (sum >> 16);
	sum = (sum & 0xffff) + (sum >> 16);
	return (__u16)sum;
}

static __always_inline __u32 csum_replace16(__u32 sum, __u16 from, __u16 to)
{
	return sum + (__u16)~from + to;
}

static __always_inline __u32 csum_replace32(__u32 sum, __u32 from, __u32 to)
{
	sum = csum_replace16(sum, (__u16)from, (__u16)to);
	return csum_replace16(sum, (__u16)(from >> 16), (__u16)(to >> 16));
}

// rewrite sets both address pairs with an incremental IP checksum (RFC 1624). With udp_csum it also updates the UDP checksum
// the same way, keeping a zero (none) zero; XDP needs that, tc leaves it to the skb helpers (udp_csum_skb).
static __always_inline void rewrite(struct iphdr *ip, struct udphdr *udp, struct tuple *to, int udp_csum)
{
	__u32 addrs = csum_replace32(0, ip->saddr, to->saddr);
	addrs = csum_replace32(addrs, ip->daddr, to->daddr);
	ip->check = ~csum_fold((__u16)~ip->check + addrs);
	if (udp_csum && udp->check) {
		__u32 sum = (__u16)~udp->check + addrs;
		sum = csum_replace16(sum, udp->source, to->sport);
		sum = csum_replace16(sum, udp->dest, to->dport);
		__u16 check = ~csum_fold(sum);
		udp->check = check ? check : 0xffff;
	}
	ip->saddr = to->saddr;
	ip->daddr = to->daddr;
	udp->source = to->sport;
	udp->dest = to->dport;
}

// udp_csum_skb moves the UDP checksum from the old tuple to the new one. The helpers handle every checksum state of the skb
// (a CHECKSUM_PARTIAL packet from a local veth holds only the pseudo-header sum; CHECKSUM_COMPLETE needs skb->csum kept).
static __always_inline void udp_csum_skb(struct __sk_buff *skb, struct tuple *from, struct tuple *to)
{
	__u64 pseudo = BPF_F_PSEUDO_HDR | BPF_F_MARK_MANGLED_0 | sizeof(__be32);
	bpf_l4_csum_replace(skb, UDP_CSUM_OFF, from->saddr, to->saddr, pseudo);
	bpf_l4_csum_replace(skb, UDP_CSUM_OFF, from->daddr, to->daddr, pseudo);
	bpf_l4_csum_replace(skb, UDP_CSUM_OFF, from->sport, to->sport, BPF_F_MARK_MANGLED_0 | sizeof(__be16));
	bpf_l4_csum_replace(skb, UDP_CSUM_OFF, from->dport, to->dport, BPF_F_MARK_MANGLED_0 | sizeof(__be16));
}

static __always_inline int fib(struct __sk_buff *skb, struct pkt *p, struct tuple *t, __u32 ifindex, __u32 flags,
			       struct bpf_fib_lookup *params)
{
	__builtin_memset(params, 0, sizeof(*params));
	params->family = AF_INET;
	params->l4_protocol = IPPROTO_UDP;
	params->sport = t->sport;
	params->dport = t->dport;
	params->tot_len = bpf_ntohs(p->ip->tot_len);
	params->tos = p->ip->tos;
	params->ipv4_src = t->saddr;
	params->ipv4_dst = t->daddr;
	params->ifindex = ifindex;
	return bpf_fib_lookup(skb, params, sizeof(*params), flags) == BPF_FIB_LKUP_RET_SUCCESS;
}

static __always_inline int load(struct __sk_buff *skb, struct pkt *p)
{
	if ((void *)(long)skb->data + PKT_LEN > (void *)(long)skb->data_end)
		bpf_skb_pull_data(skb, PKT_LEN);
	return parse((void *)(long)skb->data, (void *)(long)skb->data_end, skb->len - sizeof(struct ethhdr), p);
}

// forward rewrites a matched packet (UDP checksum through the helpers), sets the next hop's MACs and the mark, and
// redirects it out of the device the route lookup chose.
static __always_inline int forward(struct __sk_buff *skb, struct pkt *p, struct tuple *from, struct tuple *to,
				   struct bpf_fib_lookup *params, __u32 mark)
{
	rewrite(p->ip, p->udp, to, 0);
	__builtin_memcpy(p->eth->h_dest, params->dmac, ETH_ALEN);
	__builtin_memcpy(p->eth->h_source, params->smac, ETH_ALEN);
	udp_csum_skb(skb, from, to);
	skb->mark = mark;
	return bpf_redirect(params->ifindex, 0);
}

// put_back undoes wg_xdp's rewrite, so the frame is again exactly as it arrived and the slow path can take it.
static __always_inline void put_back(struct pkt *p, struct tuple *from, __be16 orig_check)
{
	rewrite(p->ip, p->udp, from, 0);
	p->udp->check = orig_check;
}

// forward_xdp_marked finishes a frame wg_xdp rewrote: the UDP checksum, then the same route and redirect as in tc mode.
static __always_inline int forward_xdp_marked(struct __sk_buff *skb, struct pkt *p, struct wg_meta *m)
{
	struct tuple from = {.saddr = m->orig_saddr, .daddr = m->orig_daddr, .sport = m->orig_sport, .dport = m->orig_dport};
	struct tuple to = {.saddr = p->ip->saddr, .daddr = p->ip->daddr, .sport = p->udp->source, .dport = p->udp->dest};
	__be16 orig_check = m->orig_check;
	struct bpf_fib_lookup params;

	// A GRO aggregate is not forwarded (as in tc mode); left rewritten, the kernel would drop it as a martian.
	if (skb->gso_size) {
		put_back(p, &from, orig_check);
		return TCX_NEXT;
	}
	// XDP computed the UDP checksum as if the frame carried a full one; the skb helpers know better (a partial checksum).
	p->udp->check = orig_check;
	if (!fib(skb, p, &to, skb->ingress_ifindex, 0, &params)) {
		put_back(p, &from, orig_check);
		count(WG_STAT_PASSED_ROUTE);
		return TCX_NEXT;
	}
	__builtin_memcpy(p->eth->h_dest, params.dmac, ETH_ALEN);
	__builtin_memcpy(p->eth->h_source, params.smac, ETH_ALEN);
	udp_csum_skb(skb, &from, &to);
	skb->mark = (skb->mark & ~CILIUM_MAGIC_MASK) | CILIUM_MAGIC_HOST;
	count(WG_STAT_FORWARDED_TO_SERVER);
	return bpf_redirect(params.ifindex, 0);
}

// Client -> server on the external device. The rewritten packet goes into Cilium's datapath like one the demux socket sent:
// host mark, then the route the kernel would take (cilium_host).
SEC("tcx/ingress")
int wg_tc_ingress(struct __sk_buff *skb)
{
	struct pkt p;

	if (load(skb, &p))
		return TCX_NEXT;

	void *data = (void *)(long)skb->data;
	struct wg_meta *m = (void *)(long)skb->data_meta;
	if ((void *)(m + 1) <= data && m->magic == WG_META_MAGIC)
		return forward_xdp_marked(skb, &p, m);
	if (skb->gso_size)
		return TCX_NEXT;

	if (p.type == WG_HANDSHAKE_INIT) {
		record_entry(&p);
		return TCX_NEXT;
	}
	if (p.type != WG_TRANSPORT || p.len < WG_TRANSPORT_MIN_LEN || !is_entry(p.ip->daddr, p.udp->dest))
		return TCX_NEXT;

	struct wg_fwd *f;
	struct wg_cfg *cfg;
	enum judgement j = judge(&p, 0, 0, &f, &cfg);
	if (j != J_FORWARD) {
		count_judgement(j);
		return j == J_RATE ? TCX_DROP : TCX_NEXT;
	}

	struct tuple from = {.saddr = p.ip->saddr, .daddr = p.ip->daddr, .sport = p.udp->source, .dport = p.udp->dest};
	struct tuple to = {.saddr = f->new_src_ip, .daddr = f->dst_ip, .sport = f->new_src_port, .dport = f->dst_port};
	struct bpf_fib_lookup params;
	if (!fib(skb, &p, &to, skb->ingress_ifindex, 0, &params)) {
		count(WG_STAT_PASSED_ROUTE);
		return TCX_NEXT;
	}
	count(WG_STAT_FORWARDED_TO_SERVER);
	return forward(skb, &p, &from, &to, &params, (skb->mark & ~CILIUM_MAGIC_MASK) | CILIUM_MAGIC_HOST);
}

// Server -> client on cilium_host (cross-node servers, after Cilium's overlay processing) and on the host-side veth of a
// same-node server (before Cilium). Only the device recorded in the session is accepted, so no other pod can inject replies.
SEC("tcx/ingress")
int wg_tc_reply(struct __sk_buff *skb)
{
	struct pkt p;

	if (skb->gso_size || load(skb, &p))
		return TCX_NEXT;
	if (p.type != WG_TRANSPORT || p.len < WG_TRANSPORT_MIN_LEN)
		return TCX_NEXT;

	struct wg_fwd *f;
	struct wg_cfg *cfg;
	enum judgement j = judge(&p, 1, skb->ifindex, &f, &cfg);
	if (j != J_FORWARD) {
		count_judgement(j);
		return j == J_RATE ? TCX_DROP : TCX_NEXT;
	}

	struct tuple from = {.saddr = p.ip->saddr, .daddr = p.ip->daddr, .sport = p.udp->source, .dport = p.udp->dest};
	struct tuple to = {.saddr = f->new_src_ip, .daddr = f->dst_ip, .sport = f->new_src_port, .dport = f->dst_port};
	struct bpf_fib_lookup params;
	if (!fib(skb, &p, &to, cfg->ext_ifindex, BPF_FIB_LOOKUP_OUTPUT, &params)) {
		count(WG_STAT_PASSED_ROUTE);
		return TCX_NEXT;
	}
	count(WG_STAT_FORWARDED_TO_CLIENT);
	return forward(skb, &p, &from, &to, &params, WG_REPLY_MARK);
}

// Our replies leave without Cilium's to-netdev program, which would otherwise create a NodePort NAT and CT entry for every
// client tuple. Everything else, the slow path's replies included, goes on to Cilium.
SEC("tcx/egress")
int wg_tc_egress_guard(struct __sk_buff *skb)
{
	if (skb->mark != WG_REPLY_MARK)
		return TCX_NEXT;

	struct ethhdr eth;
	struct iphdr ip;
	struct udphdr udp;
	if (bpf_skb_load_bytes(skb, 0, &eth, sizeof(eth)) ||
	    bpf_skb_load_bytes(skb, sizeof(eth), &ip, sizeof(ip)) ||
	    bpf_skb_load_bytes(skb, sizeof(eth) + sizeof(ip), &udp, sizeof(udp)))
		return TCX_NEXT;
	if (eth.h_proto != bpf_htons(ETH_P_IP) || ip.ihl != 5 || ip.protocol != IPPROTO_UDP)
		return TCX_NEXT;
	return is_entry(ip.saddr, udp.source) ? TCX_PASS : TCX_NEXT;
}

// Client -> server in XDP: judge and rewrite here, drop over-rate packets early, and hand the frame to wg_tc_ingress through
// the metadata (generic XDP cannot enter Cilium's datapath itself). The metadata room is made before the judgement, so a
// driver without it costs no token: the frame passes untouched and wg_tc_ingress handles it as in tc mode. Frames that are
// not forwarded pass untouched and uncounted: wg_tc_ingress judges them again and counts them once.
SEC("xdp")
int wg_xdp(struct xdp_md *ctx)
{
	struct pkt p;

	if (parse((void *)(long)ctx->data, (void *)(long)ctx->data_end, frame_len(ctx), &p))
		return XDP_PASS;
	if (p.type != WG_TRANSPORT || p.len < WG_TRANSPORT_MIN_LEN || !is_entry(p.ip->daddr, p.udp->dest))
		return XDP_PASS;

	if (bpf_xdp_adjust_meta(ctx, -(int)sizeof(struct wg_meta))) {
		count(WG_STAT_PASSED_META);
		return XDP_PASS;
	}
	void *data = (void *)(long)ctx->data;
	struct wg_meta *m = (void *)(long)ctx->data_meta;
	if ((void *)(m + 1) > data || parse(data, (void *)(long)ctx->data_end, frame_len(ctx), &p)) {
		bpf_xdp_adjust_meta(ctx, (int)sizeof(struct wg_meta));
		return XDP_PASS;
	}

	struct wg_fwd *f;
	struct wg_cfg *cfg;
	enum judgement j = judge(&p, 0, 0, &f, &cfg);
	if (j != J_FORWARD) {
		bpf_xdp_adjust_meta(ctx, (int)sizeof(struct wg_meta));
		if (j != J_RATE)
			return XDP_PASS;
		count(WG_STAT_DROPPED_RATE);
		return XDP_DROP;
	}

	struct wg_meta orig = {
		.magic = WG_META_MAGIC,
		.orig_saddr = p.ip->saddr,
		.orig_daddr = p.ip->daddr,
		.orig_sport = p.udp->source,
		.orig_dport = p.udp->dest,
		.orig_check = p.udp->check,
	};
	struct tuple to = {.saddr = f->new_src_ip, .daddr = f->dst_ip, .sport = f->new_src_port, .dport = f->dst_port};
	rewrite(p.ip, p.udp, &to, 1);
	*m = orig;
	return XDP_PASS;
}

char __license[] SEC("license") = "GPL";
