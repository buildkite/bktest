package rspec

import "testing"

func TestExamplePrimarySelector(t *testing.T) {
	for _, tc := range []struct {
		name, id, filePath, want string
	}{
		{"shared example", "./spec/specs_with_shared_examples_spec.rb[1:1:1]", "./spec/shared_examples.rb", "./spec/specs_with_shared_examples_spec.rb"},
		{"ordinary example", "./spec/a_spec.rb[1:2]", "./spec/a_spec.rb", "./spec/a_spec.rb"},
		{"bracket in filename", "./spec/a[variant]_spec.rb[1:2]", "./spec/a[variant]_spec.rb", "./spec/a[variant]_spec.rb"},
		{"missing id", "", "./spec/a_spec.rb", "./spec/a_spec.rb"},
		{"invalid id", "./spec/a_spec.rb[not-a-location]", "./spec/a_spec.rb", "./spec/a_spec.rb"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := (Example{ID: tc.id, FilePath: tc.filePath}).PrimarySelector(); got != tc.want {
				t.Fatalf("primary selector=%q, want %q", got, tc.want)
			}
		})
	}
}
