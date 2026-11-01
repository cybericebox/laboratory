package nodeagent

import (
	"context"

	"sigs.k8s.io/controller-runtime/pkg/client"

	laboratoryv1alpha1 "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
)

// The guards of a port operation read the objects they decide on straight from the API server (directReader). Two lookups of
// them are not guards but searches over the whole cluster, repeated for every port operation: the LabGroups that own a
// pod's namespace, and the Connections that share a name. They are served from the informer cache first; the cache can only
// lag by milliseconds, so a verdict that would refuse (no owning group, an ambiguous name) is read again from the API server
// before it counts. A positive verdict from the cache is what a guard read a moment earlier could have said as well: the
// pod, its ReplicaSet and its Deployment, which carry the identity, are still read directly.

// groupsOfNamespace lists the LabGroups whose namespace is ns.
func groupsOfNamespace(ctx context.Context, cached, direct client.Reader, ns string, accept func([]laboratoryv1alpha1.LabGroup) (bool, error)) (bool, error) {
	for _, rd := range []client.Reader{cached, direct} {
		var groups laboratoryv1alpha1.LabGroupList
		if err := rd.List(ctx, &groups); err != nil {
			return false, err
		}
		var inNS []laboratoryv1alpha1.LabGroup
		for i := range groups.Items {
			if laboratoryv1alpha1.LabGroupNamespaceOf(&groups.Items[i]) == ns {
				inNS = append(inNS, groups.Items[i])
			}
		}
		ok, err := accept(inNS)
		if err != nil || ok {
			return ok, err
		}
		if rd == direct {
			break
		}
	}
	return false, nil
}

// connectionNameUnique says that exactly one Connection of the cluster has the name.
func connectionNameUnique(ctx context.Context, cached, direct client.Reader, name string) (bool, error) {
	for i, rd := range []client.Reader{cached, direct} {
		var all laboratoryv1alpha1.ConnectionList
		if err := rd.List(ctx, &all); err != nil {
			return false, err
		}
		matches := 0
		for j := range all.Items {
			if all.Items[j].Name == name {
				matches++
			}
		}
		if matches == 1 {
			return true, nil
		}
		if i == 1 {
			break
		}
	}
	return false, nil
}
