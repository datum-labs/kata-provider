package resourcemetrics

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"sigs.k8s.io/yaml"
)

// TestShippedRules tests the expressions from the actual provider VMRule.
func TestShippedRules(t *testing.T) {
	promtool, err := exec.LookPath("promtool")
	if err != nil {
		t.Skip("promtool is required to run resource metric rule regressions")
	}
	contents, err := os.ReadFile("../../config/components/resource-metrics/instance-resource-rules.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var rule struct {
		Spec map[string]any `json:"spec"`
	}
	if err := yaml.Unmarshal(contents, &rule); err != nil {
		t.Fatal(err)
	}
	generated, err := yaml.Marshal(rule.Spec)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "rules.yaml"), generated, 0600); err != nil {
		t.Fatal(err)
	}
	tests, err := os.ReadFile("rules.test.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "tests.yaml"), tests, 0600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(promtool, "test", "rules", "tests.yaml")
	cmd.Dir = dir
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("rule regressions: %v\n%s", err, output)
	}
}
