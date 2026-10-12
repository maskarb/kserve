package kservemodule

import (
	"os"
	"path/filepath"
	"testing"

	. "github.com/onsi/gomega"
)

func TestConfidentialStorageHelperImageOverride(t *testing.T) {
	g := NewWithT(t)
	dir := t.TempDir()
	paramsFile := filepath.Join(dir, "params.env")
	g.Expect(os.WriteFile(paramsFile, []byte("osc-storage-helper=registry.redhat.io/helper:1.13.1\n"), 0o600)).To(Succeed())
	t.Setenv("RELATED_IMAGE_OSC_STORAGE_HELPER_IMAGE", "registry.example.com/helper@sha256:1234")
	g.Expect(applyParams(dir, kserveImageParamMap)).To(Succeed())
	params, err := parseParams(paramsFile)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(params["osc-storage-helper"]).To(Equal("registry.example.com/helper@sha256:1234"))
}
