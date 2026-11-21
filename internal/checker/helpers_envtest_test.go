package checker

import (
	coordinationv1 "k8s.io/api/coordination/v1"
	"k8s.io/client-go/rest"
)

type coordLeaseList struct{ list coordinationv1.LeaseList }

var testRestConfig *rest.Config
