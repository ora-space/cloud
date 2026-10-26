package main

import "testing"

func TestDeploymentEgressPath(t *testing.T) {
	cases := []struct {
		name string
		env  map[string]string
		want string
	}{
		{"neither", nil, ""},
		{"shared only", map[string]string{"ORA_DEPLOYMENT_EGRESS_FILE": "/etc/egress.json"}, "/etc/egress.json"},
		{"executor only", map[string]string{"REPO_EXECUTOR_DEPLOYMENT_EGRESS_FILE": "/etc/executor.json"}, "/etc/executor.json"},
		{"executor wins", map[string]string{
			"REPO_EXECUTOR_DEPLOYMENT_EGRESS_FILE": " /etc/executor.json ",
			"ORA_DEPLOYMENT_EGRESS_FILE":           "/etc/shared.json",
		}, "/etc/executor.json"},
		{"blank executor falls back", map[string]string{
			"REPO_EXECUTOR_DEPLOYMENT_EGRESS_FILE": "  ",
			"ORA_DEPLOYMENT_EGRESS_FILE":           "/etc/shared.json",
		}, "/etc/shared.json"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := deploymentEgressPath(func(key string) string { return tc.env[key] })
			if got != tc.want {
				t.Fatalf("deploymentEgressPath() = %q, want %q", got, tc.want)
			}
		})
	}
}
