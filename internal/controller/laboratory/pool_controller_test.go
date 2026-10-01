package laboratory

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"sigs.k8s.io/controller-runtime/pkg/client"

	allocationv1alpha1 "github.com/cybericebox/laboratory/api/allocation/v1alpha1"
	poolpkg "github.com/cybericebox/laboratory/pkg/api/pool"
)

var _ = Describe(
	"Pool allocator", func() {
		const (
			testNS     = "default"
			testPrefix = "test-pool"
		)

		BeforeEach(
			func() {
				var list allocationv1alpha1.PoolList
				Expect(
					k8sClient.List(
						ctx, &list,
						client.InNamespace(testNS),
						client.MatchingLabels{poolpkg.PoolGroupLabel: testPrefix},
					),
				).To(Succeed())
				for i := range list.Items {
					Expect(k8sClient.Delete(ctx, &list.Items[i])).To(Succeed())
				}
			},
		)

		It(
			"allocates sequential (distinct) indices", func() {
				alloc := poolpkg.NewAllocator(k8sClient, testPrefix, testNS, 10)

				idx1, err := alloc.AllocateIndex(ctx)
				Expect(err).NotTo(HaveOccurred())

				idx2, err := alloc.AllocateIndex(ctx)
				Expect(err).NotTo(HaveOccurred())

				Expect(idx2).NotTo(Equal(idx1))
			},
		)

		It(
			"releases an index and reallocates the same slot", func() {
				alloc := poolpkg.NewAllocator(k8sClient, testPrefix, testNS, 10)

				idx, err := alloc.AllocateIndex(ctx)
				Expect(err).NotTo(HaveOccurred())

				Expect(alloc.ReleaseIndex(ctx, idx)).To(Succeed())

				idx2, err := alloc.AllocateIndex(ctx)
				Expect(err).NotTo(HaveOccurred())
				Expect(idx2).To(Equal(idx))
			},
		)

		It(
			"creates a second pool when the first is full", func() {
				// size=2, offset=0: bit 0 reserved → only 1 free slot in the first pool.
				alloc := poolpkg.NewAllocator(k8sClient, testPrefix, testNS, 2)

				idx1, err := alloc.AllocateIndex(ctx)
				Expect(err).NotTo(HaveOccurred())
				// First pool is now full.

				idx2, err := alloc.AllocateIndex(ctx)
				Expect(err).NotTo(HaveOccurred())
				// Second pool starts at offset 2; idx2 must be strictly greater.
				Expect(idx2).To(BeNumerically(">", idx1))
			},
		)
	},
)
