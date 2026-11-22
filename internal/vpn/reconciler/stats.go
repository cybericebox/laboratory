package reconciler

import (
	"context"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"golang.zx2c4.com/wireguard/wgctrl/wgtypes"

	laboratoryv1alpha1 "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
)

// peerDevice reads the WireGuard device (vpn.WGManager).
type peerDevice interface {
	Device() (*wgtypes.Device, error)
}

// RunStats periodically reads WireGuard peer stats and writes them to LabGroupClient status in namespace: every peer each statsEvery,
// and, each handshakeEvery, only the peers whose handshake is newer than their status shows. The handshake is what tells a connected
// client (the daemon reads it), so a client that connects shows as connected within handshakeEvery instead of up to statsEvery later;
// the short check reads the local device and the cache, and writes a client about once per WireGuard handshake (every two minutes of
// an active tunnel). handshakeEvery <= 0 turns the short check off.
func RunStats(ctx context.Context, c client.Client, wg peerDevice, namespace string, statsEvery, handshakeEvery time.Duration) {
	ticker := time.NewTicker(statsEvery)
	defer ticker.Stop()
	var handshakes <-chan time.Time
	if handshakeEvery > 0 {
		hs := time.NewTicker(handshakeEvery)
		defer hs.Stop()
		handshakes = hs.C
	}
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			publishPeerStats(ctx, c, wg, namespace, false)
		case <-handshakes:
			publishPeerStats(ctx, c, wg, namespace, true)
		}
	}
}

// publishPeerStats writes the statistics of every peer, or with newHandshakesOnly only of the peers whose handshake moved (status
// keeps whole seconds, so the comparison is in seconds).
func publishPeerStats(ctx context.Context, c client.Client, wg peerDevice, namespace string, newHandshakesOnly bool) {
	dev, err := wg.Device()
	if err != nil {
		return
	}
	var lgcList laboratoryv1alpha1.LabGroupClientList
	if err := c.List(ctx, &lgcList, client.InNamespace(namespace)); err != nil {
		return
	}
	for _, peer := range dev.Peers {
		pubKey := peer.PublicKey.String()
		for i := range lgcList.Items {
			if lgcList.Items[i].Spec.PublicKey == pubKey {
				if newHandshakesOnly && (peer.LastHandshakeTime.IsZero() ||
					peer.LastHandshakeTime.Unix() <= lgcList.Items[i].Status.Statistics.LastHandshake.Unix()) {
					break
				}
				patch := client.MergeFrom(lgcList.Items[i].DeepCopy())
				lgcList.Items[i].Status.Statistics = laboratoryv1alpha1.LabGroupClientStatistics{
					LastHandshake: metav1.NewTime(peer.LastHandshakeTime),
					RxBytes:       peer.ReceiveBytes,
					TxBytes:       peer.TransmitBytes,
				}
				_ = c.Status().Patch(ctx, &lgcList.Items[i], patch)
				break
			}
		}
	}
}
