#!/usr/bin/env bash
# Bash runner plugin for Cypress.
set -uo pipefail
# shellcheck source=../../lib/runner-plugin.bash
source "${BKTEST_RUNNER_PLUGIN_LIB:-$(dirname "${BASH_SOURCE[0]}")/../../lib/runner-plugin.bash}"

plugin_initialize() {
  jq '.config
    | .test_command = (.test_command // "npx cypress run --spec {{testExamples}}")
    | .retry_test_command = (.retry_test_command // .test_command)
    | .test_file_pattern = (.test_file_pattern // "**/*.cy.{js,jsx,ts,tsx}")' <<<"$BK_PARAMS" | bk_save_config
  jq '{
    protocol_version: 1,
    name: "Cypress",
    features: {split_by_file: true, filter_test_files: true, split_by_selector: true},
    result_path: (.result_path // ""),
    location_prefix: (.location_prefix // "")
  }' "$BK_STATE_DIR/config.json"
}

plugin_discover() {
  bk_discover_files
}

# Every spec file goes in one --spec argument, joined by commas.
plugin_command() {
  local specs
  local -a words
  bk_test_command words
  specs=$(bk_paths | paste -sd, -)
  bk_splice words '{{testExamples}}' "$specs" || words+=(--spec "$specs")
  bk_command_result "${words[@]}"
}

# Cypress has no report: only its exit status counts.
plugin_interpret() {
  echo '{}'
}

bk_serve
