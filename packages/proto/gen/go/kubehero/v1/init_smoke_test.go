package kuberov1

import "testing"

func TestDescriptorInit(t *testing.T) {
	if File_kubehero_v1_collector_proto == nil {
		t.Fatal("collector proto file descriptor is nil")
	}
}
