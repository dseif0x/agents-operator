package reconcile

import (
	"sort"

	"k8s.io/apimachinery/pkg/util/intstr"
)

func intstrFromInt(i int) intstr.IntOrString { return intstr.FromInt32(int32(i)) }

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
