package main

import (
	"flag"
	"strings"
	"testing"
)

// The flags of the README, with their defaults. kumaco has no webhook, so
// none of the --webhook-* flags.
//
// README: #configuration
func TestTheCommandLineIsTheDocumentedOne(t *testing.T) {
	flags := flag.NewFlagSet("kumaco", flag.ContinueOnError)
	bindFlags(flags)

	want := map[string]string{
		"controller-name":       "kumaco",
		"node-selector":         "",
		"window-label-selector": "",
		"condition-prefix":      "kumaco.kured.dev/",
		"heartbeat-interval":    "5m0s",
		"kube-api-qps":          "50",
		"kube-api-burst":        "100",

		"metrics-bind-address":      "0",
		"metrics-secure":            "true",
		"metrics-cert-path":         "",
		"metrics-cert-name":         "tls.crt",
		"metrics-cert-key":          "tls.key",
		"enable-http2":              "false",
		"health-probe-bind-address": ":8081",
		"leader-elect":              "false",

		"zap-devel":            "true",
		"zap-encoder":          "",
		"zap-log-level":        "",
		"zap-stacktrace-level": "",
		"zap-time-encoding":    "",
	}
	flags.VisitAll(func(f *flag.Flag) {
		wantDefault, documented := want[f.Name]
		if !documented {
			t.Errorf("--%s is not documented in the README", f.Name)
			return
		}
		if f.DefValue != wantDefault {
			t.Errorf("--%s defaults to %q, want %q", f.Name, f.DefValue, wantDefault)
		}
		if f.Usage == "" {
			t.Errorf("--%s has no usage", f.Name)
		}
		delete(want, f.Name)
	})
	for name := range want {
		t.Errorf("--%s is documented in the README, but not defined", name)
	}
}

// README: #configuration
func TestTheCommandLineIsValidated(t *testing.T) {
	for _, tt := range []struct {
		name string
		args []string
		// want is a part of the error; empty when the command line is valid.
		want string
	}{
		{name: "the defaults"},
		{name: "selectors", args: []string{"--node-selector=team=a,!spare", "--window-label-selector=MaintenanceWindowTeam=A"}},
		{name: "an invalid node selector", args: []string{"--node-selector=team in"}, want: "--node-selector"},
		{name: "an invalid window selector", args: []string{"--window-label-selector==="}, want: "--window-label-selector"},
		{name: "an empty controller name", args: []string{"--controller-name="}, want: "--controller-name"},
		{name: "an empty reserved prefix", args: []string{"--condition-prefix="}, want: "--condition-prefix"},
		{name: "no heartbeat", args: []string{"--heartbeat-interval=0"}, want: "--heartbeat-interval"},
		{name: "no request rate", args: []string{"--kube-api-qps=0"}, want: "--kube-api-qps"},
		{name: "no request burst", args: []string{"--kube-api-burst=0"}, want: "--kube-api-burst"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			flags := flag.NewFlagSet("kumaco", flag.ContinueOnError)
			cfg, _ := bindFlags(flags)
			if err := flags.Parse(tt.args); err != nil {
				t.Fatal(err)
			}
			nodes, windows, err := cfg.validate()
			switch {
			case tt.want == "" && err != nil:
				t.Errorf("validate: %v", err)
			case tt.want == "" && (nodes == nil || windows == nil):
				t.Errorf("validate returned no selector")
			case tt.want != "" && (err == nil || !strings.Contains(err.Error(), tt.want)):
				t.Errorf("validate: %v, want an error naming %s", err, tt.want)
			}
		})
	}
}
