package main

import (
	"reflect"
	"testing"
)

func TestProject(t *testing.T) {
	known := map[string]struct{}{
		"cdu":   {},
		"pod":   {},
		"rack":  {},
		"pdu":   {},
		"meter": {},
	}

	cases := []struct {
		name    string
		sa      SeedAsset
		wantErr string // empty = no error
		want    map[string]any
	}{
		{
			name: "normal",
			sa: SeedAsset{
				Path: "sgp01.pod002.cdu000",
				Type: "cdu",
				Attributes: map[string]any{
					"model":          "DC45",
					"rated_power_kw": 1240,
				},
			},
			want: map[string]any{
				"type":           "cdu",
				"model":          "DC45",
				"rated_power_kw": 1240,
			},
		},
		{
			name: "empty path",
			sa: SeedAsset{
				Path: "",
				Type: "cdu",
			},
			wantErr: "empty path",
		},
		{
			name: "unknown type",
			sa: SeedAsset{
				Path: "sgp01.pod002.qbox000",
				Type: "qbox",
			},
			wantErr: "not in protocol/types.yaml",
		},
		{
			name: "attributes contains type",
			sa: SeedAsset{
				Path: "sgp01.pod002.cdu000",
				Type: "cdu",
				Attributes: map[string]any{
					"type":  "rack",
					"model": "X",
				},
			},
			wantErr: "must not contain key",
		},
		{
			name: "empty type",
			sa: SeedAsset{
				Path: "sgp01.pod002.cdu000",
				Type: "",
			},
			wantErr: "empty type",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Project(tc.sa, known)
			if tc.wantErr != "" {
				if err == nil {
					t.Fatalf("want error containing %q, got nil", tc.wantErr)
				}
				if !contains(err.Error(), tc.wantErr) {
					t.Fatalf("error %q does not contain %q", err.Error(), tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got.Path != tc.sa.Path {
				t.Errorf("Path: want %q got %q", tc.sa.Path, got.Path)
			}
			if got.ResourceVersion != 0 {
				t.Errorf("ResourceVersion: want 0 got %d", got.ResourceVersion)
			}
			if !reflect.DeepEqual(got.Spec, tc.want) {
				t.Errorf("Spec: want %v got %v", tc.want, got.Spec)
			}
		})
	}
}

func contains(s, sub string) bool {
	return len(sub) == 0 || (len(s) >= len(sub) && (indexOf(s, sub) >= 0))
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
