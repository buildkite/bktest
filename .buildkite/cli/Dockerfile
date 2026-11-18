# Keep the browser image's system accounts and package database together.
FROM docker.io/library/ruby:4.0.7-slim-bookworm AS ruby
FROM docker.io/library/python:3.15.2-bookworm AS python
FROM docker.io/library/golang:1.27.3-bookworm AS go

FROM docker.io/cypress/included:14.5.3
ENTRYPOINT []
COPY --from=ruby /usr/local/ /usr/local/
COPY --from=python /usr/local/ /usr/local/
COPY --from=go /usr/local/go/ /usr/local/go/
ENV PATH="/usr/local/go/bin:$PATH"

RUN apt-get update && apt-get install -y --no-install-recommends \
      bats build-essential git libgmp10 libyaml-0-2 \
    && rm -rf /var/lib/apt/lists/*
RUN gem install --no-document rspec cucumber base64 concurrent-ruby \
    && gem install --no-document bigdecimal -v 3.2.0
# The Go runner fixtures execute gotestsum even though CI calls go test directly.
RUN GOBIN=/usr/local/bin go install gotest.tools/gotestsum@v1.8.0
RUN git config --system --add safe.directory /work
