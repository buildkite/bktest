#!/usr/bin/env bash
# Bash runner plugin for Cypress.
# shellcheck source-path=SCRIPTDIR disable=SC2317 # bk_main calls the plugin_ functions

# shellcheck source=../../lib/runner-plugin.bash
source "${BKTEST_RUNNER_PLUGIN_LIB:-$(dirname "${BASH_SOURCE[0]}")/../../lib/runner-plugin.bash}"

plugin_configure() {
  bk_request '{
    name: "Cypress",
    config: (.config
      | bk_default("test_command"; "npx cypress run --spec {{testExamples}}")
      | bk_default("retry_test_command"; .test_command)
      | bk_default("test_file_pattern"; "**/*.cy.{js,jsx,ts,tsx}")),
    features: {split_by_file: true, filter_test_files: true, split_by_selector: true}
  }'
}

# Every spec file goes in one --spec argument, joined by commas. Cypress has
# no report bktest reads, so only its exit status counts and there is no
# interpret-attempt.
plugin_prepare_attempt() {
  # shellcheck disable=SC2016
  bk_request '(bk_paths | join(",")) as $specs
    | bk_test_command | bk_words
    | if bk_index("{{testExamples}}") == null then . + ["--spec", $specs]
      else bk_splice("{{testExamples}}"; [$specs]) end
    | {argv: .}'
}

bk_main "$@"
