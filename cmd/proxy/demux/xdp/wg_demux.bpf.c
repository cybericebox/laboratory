// cmd/proxy/demux/xdp/wg_demux.bpf.c
//go:build ignore

#include <linux/bpf.h>
#include <linux/if_ether.h>
#include <linux/ip.h>
#include <linux/in.h>
#include <linux/udp.h>
#include <bpf/bpf_helpers.h>
#include <bpf/bpf_endian.h>

#define WG_PORT      bpf_htons(51820)
#define WG_TYPE_DATA 4

/* Fragment flag bits in ip->frag_off */
#ifndef IP_MF
#define IP_MF     0x2000
#endif
#ifndef IP_OFFSET
#define IP_OFFSET 0x1FFF
#endif

struct dst_entry {
	__u32 ip;
	__u16 port;
	__u16 pad;
};

struct xdp_cfg {
	__u32 proxy_ip;
	__u16 proxy_port;
	__u8  gw_mac[ETH_ALEN];
	__u8  pad[2];
};

struct {
	__uint(type, BPF_MAP_TYPE_HASH);
	__uint(max_entries, 65536);
	__type(key, __u32);
	__type(value, struct dst_entry);
} wg_sessions SEC(".maps");

struct {
	__uint(type, BPF_MAP_TYPE_ARRAY);
	__uint(max_entries, 1);
	__type(key, __u32);
	__type(value, struct xdp_cfg);
} xdp_cfg_map SEC(".maps");

static __always_inline __be16 csum_fold_helper(__u32 csum)
{
	csum = (csum & 0xffff) + (csum >> 16);
	csum = (csum & 0xffff) + (csum >> 16);
	return (__be16)~csum;
}

SEC("xdp")
int wg_demux(struct xdp_md *ctx)
{
	void *data_end = (void *)(long)ctx->data_end;
	void *data     = (void *)(long)ctx->data;

	/* Parse Ethernet */
	struct ethhdr *eth = data;
	if ((void *)(eth + 1) > data_end)
		return XDP_PASS;
	if (eth->h_proto != bpf_htons(ETH_P_IP))
		return XDP_PASS;

	/* Parse IPv4 — no fragments, no options */
	struct iphdr *ip = (void *)(eth + 1);
	if ((void *)(ip + 1) > data_end)
		return XDP_PASS;
	if (ip->ihl != 5 || ip->protocol != IPPROTO_UDP)
		return XDP_PASS;
	if (ip->frag_off & bpf_htons(IP_MF | IP_OFFSET))
		return XDP_PASS;

	/* Parse UDP */
	struct udphdr *udp = (void *)(ip + 1);
	if ((void *)(udp + 1) > data_end)
		return XDP_PASS;
	if (udp->dest != WG_PORT)
		return XDP_PASS;

	/* Parse WireGuard header: type byte + 3 reserved + receiver_index (4 bytes) */
	__u8 *wg = (void *)(udp + 1);
	if (wg + 8 > (__u8 *)data_end)
		return XDP_PASS;
	if (wg[0] != WG_TYPE_DATA)
		return XDP_PASS;

	/* receiver_index is bytes 4-7 little-endian */
	__u32 ri = (__u32)wg[4] | ((__u32)wg[5] << 8) |
	           ((__u32)wg[6] << 16) | ((__u32)wg[7] << 24);

	struct dst_entry *dst = bpf_map_lookup_elem(&wg_sessions, &ri);
	if (!dst)
		return XDP_PASS;

	__u32 cfg_key = 0;
	struct xdp_cfg *cfg = bpf_map_lookup_elem(&xdp_cfg_map, &cfg_key);
	if (!cfg)
		return XDP_PASS;

	/* Rewrite Ethernet destination to gateway MAC */
	__builtin_memcpy(eth->h_dest, cfg->gw_mac, ETH_ALEN);

	/* Rewrite IP src/dst and update checksum incrementally */
	__u32 old_src = ip->saddr;
	__u32 old_dst = ip->daddr;
	ip->saddr = cfg->proxy_ip;
	ip->daddr = dst->ip;

	__u32 from[2] = {old_src, old_dst};
	__u32 to[2]   = {ip->saddr, ip->daddr};
	ip->check = csum_fold_helper(
		bpf_csum_diff((__be32 *)from, 8, (__be32 *)to, 8, ~(__u32)ip->check));

	/* Rewrite UDP src/dst; zero checksum (valid for IPv4 per RFC 768) */
	udp->source = cfg->proxy_port;
	udp->dest   = dst->port;
	udp->check  = 0;

	return XDP_TX;
}

char __license[] SEC("license") = "GPL";
