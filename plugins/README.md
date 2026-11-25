# Runner plugins

Each built-in bktest runner as a Bash plugin and a Go plugin. See
[docs/runner-plugins.md](../docs/runner-plugins.md) for the protocol, how to
write a plugin, and known differences from the built-in runners.

| Runner | Bash | Go |
| --- | --- | --- |
| Cucumber | [cucumber/bash](cucumber/bash/plugin.bash) | [cucumber/go](cucumber/go/main.go) |
| Cypress | [cypress/bash](cypress/bash/plugin.bash) | [cypress/go](cypress/go/main.go) |
| go test | [gotest/bash](gotest/bash/plugin.bash) | [gotest/go](gotest/go/main.go) |
| Jest | [jest/bash](jest/bash/plugin.bash) | [jest/go](jest/go/main.go) |
| Playwright | [playwright/bash](playwright/bash/plugin.bash) | [playwright/go](playwright/go/main.go) |
| pytest | [pytest/bash](pytest/bash/plugin.bash) | [pytest/go](pytest/go/main.go) |
| RSpec | [rspec/bash](rspec/bash/plugin.bash) | [rspec/go](rspec/go/main.go) |
| Vitest | [vitest/bash](vitest/bash/plugin.bash) | [vitest/go](vitest/go/main.go) |

```sh
plugins/build.sh                          # build the Go plugins in place
bktest run --plugin plugins/rspec/bash    # or plugins/rspec/go
```

The Bash plugins share [lib/runner-plugin.bash](lib/runner-plugin.bash) and
need Bash 4 and `jq` 1.6 or later; pytest and go test JUnit XML also need `python3`. The Go
plugins use [pkg/runnerplugin](../pkg/runnerplugin) and nothing else from
bktest.
