package cli

import (
	"errors"
	"testing"
)

func TestResolveImage(t *testing.T) {
	tests := []struct {
		name     string
		flag     string
		settings string
		want     string
		wantErr  error
	}{
		{name: "flag wins over settings", flag: "flag-img", settings: "settings-img", want: "flag-img"},
		{name: "flag used when settings empty", flag: "flag-img", want: "flag-img"},
		{name: "settings used when no flag", settings: "settings-img", want: "settings-img"},
		{name: "both empty", wantErr: errNoImage},
		{name: "whitespace flag falls back to settings", flag: "  \t", settings: "settings-img", want: "settings-img"},
		{name: "whitespace flag with empty settings", flag: "   ", wantErr: errNoImage},
		{name: "values are trimmed", flag: " flag-img ", want: "flag-img"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := resolveImage(tt.flag, tt.settings)
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("err = %v, want %v", err, tt.wantErr)
			}
			if got != tt.want {
				t.Errorf("image = %q, want %q", got, tt.want)
			}
		})
	}
}
