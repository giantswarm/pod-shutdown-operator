package controller

import (
	"testing"

	shutdownv1alpha1 "github.com/giantswarm/pod-shutdown-operator/api/v1alpha1"
)

func TestResult(t *testing.T) {
	info := shutdownv1alpha1.Finding{Severity: shutdownv1alpha1.SeverityInfo}
	warning := shutdownv1alpha1.Finding{Severity: shutdownv1alpha1.SeverityWarning}
	errorFinding := shutdownv1alpha1.Finding{Severity: shutdownv1alpha1.SeverityError}
	tests := map[string]struct {
		findings []shutdownv1alpha1.Finding
		want     shutdownv1alpha1.PlanResult
	}{
		"no findings":         {want: shutdownv1alpha1.PlanResultOK},
		"only Info":           {findings: []shutdownv1alpha1.Finding{info, info}, want: shutdownv1alpha1.PlanResultOK},
		"Info and a Warning":  {findings: []shutdownv1alpha1.Finding{info, warning}, want: shutdownv1alpha1.PlanResultWarnings},
		"a Warning and Error": {findings: []shutdownv1alpha1.Finding{warning, errorFinding, info}, want: shutdownv1alpha1.PlanResultErrors},
	}
	for name, tt := range tests {
		if got := result(tt.findings); got != tt.want {
			t.Errorf("%s: result() = %s, want %s", name, got, tt.want)
		}
	}
}
