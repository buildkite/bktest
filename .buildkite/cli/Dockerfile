# Same Linux fixture runtimes as the old client CI; the collector code is
# installed from this checkout at test time, not from a registry.
FROM ruby:4.0.7-slim-bookworm AS ruby
FROM cypress/included:14.5.3 AS cypress
FROM python:3.15.2-bookworm AS python

FROM golang:1.27.3-bookworm
COPY --from=ruby / /
COPY --from=cypress / /
COPY --from=python / /

RUN apt-get update && apt-get install -y --no-install-recommends bats \
    && rm -rf /var/lib/apt/lists/*
RUN gem install --no-document rspec cucumber base64 concurrent-ruby \
    && gem install --no-document bigdecimal -v 3.2.0
RUN git config --system --add safe.directory /work
