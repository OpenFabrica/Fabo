package guestwire

import (
	"context"
	"fmt"
	"net/netip"
)

const KubernetesRoutesMessage Kind = 80

var kubernetesRanges = [2]netip.Prefix{netip.MustParsePrefix("10.42.0.0/16"), netip.MustParsePrefix("10.43.0.0/16")}

// KubernetesRanges are the pod and service CIDRs of Fabo's k3s node.
func KubernetesRanges() [2]netip.Prefix { return kubernetesRanges }

func KubernetesAddress(ip netip.Addr) bool {
	return kubernetesRanges[0].Contains(ip) || kubernetesRanges[1].Contains(ip)
}

// KubernetesRoutes asks the guest to route the cluster ranges via the live
// node and remove the routes when that node's network closes.
func (s *Session) KubernetesRoutes(ctx context.Context, node string) error {
	var e encoder
	e.text(node)
	if e.err != nil {
		return e.err
	}
	reply, err := s.request(ctx, Frame{Kind: KubernetesRoutesMessage, Payload: e.payload}, AckMessage, "", nil, 0)
	if err != nil {
		return err
	}
	if len(reply.Payload) != 0 {
		return fmt.Errorf("invalid Kubernetes route acknowledgment")
	}
	return nil
}
