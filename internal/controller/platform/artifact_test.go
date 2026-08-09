package platform

import (
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/rusik69/paas/api/v1alpha1"
)

func TestParsePackages_ReadsAWellFormedFile(t *testing.T) {
	t.Parallel()

	got, err := ParsePackages([]byte(`
packages:
  - name: cnpg-migrate
    chart: cnpg-migrations
    version: "1.27.0"
    stage: migration
  - name: cnpg
    chart: cnpg
    version: "1.27.0"
    stage: component
`))
	if err != nil {
		t.Fatalf("ParsePackages: %v", err)
	}

	want := []Entry{
		{Name: "cnpg-migrate", Chart: "cnpg-migrations", Version: "1.27.0", Stage: v1alpha1.StageMigration},
		{Name: "cnpg", Chart: "cnpg", Version: "1.27.0", Stage: v1alpha1.StageComponent},
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("entries differ (-want +got):\n%s", diff)
	}
}

// Each case names the specific rejection, not merely that an error occurred: a
// malformed release must say which entry is wrong, because the person reading
// it is looking at a file they did not write.
func TestParsePackages_Rejections(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name, in, want string
	}{
		{
			name: "not yaml",
			in:   "packages: [unterminated\n",
			want: "parse packages.yaml",
		},
		{
			name: "no packages",
			in:   "packages: []\n",
			want: "declares no packages",
		},
		{
			name: "unknown field",
			in:   "packages:\n  - name: a\n    chart: a\n    version: \"1\"\n    stage: component\n    typo: x\n",
			want: "parse packages.yaml",
		},
		{
			name: "missing name",
			in:   "packages:\n  - chart: a\n    version: \"1\"\n    stage: component\n",
			want: "no name",
		},
		{
			name: "missing chart",
			in:   "packages:\n  - name: a\n    version: \"1\"\n    stage: component\n",
			want: `"a" has no chart`,
		},
		{
			name: "missing version",
			in:   "packages:\n  - name: a\n    chart: a\n    stage: component\n",
			want: `"a" has no version`,
		},
		{
			name: "unknown stage",
			in:   "packages:\n  - name: a\n    chart: a\n    version: \"1\"\n    stage: later\n",
			want: `stage "later"`,
		},
		{
			name: "duplicate name",
			in: "packages:\n  - name: a\n    chart: a\n    version: \"1\"\n    stage: component\n" +
				"  - name: a\n    chart: b\n    version: \"2\"\n    stage: component\n",
			want: "declared twice",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			_, err := ParsePackages([]byte(tc.in))
			if err == nil {
				t.Fatalf("ParsePackages accepted %s", tc.name)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("err = %q, want it to contain %q", err, tc.want)
			}
		})
	}
}
