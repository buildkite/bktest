FROM alpine

# Require the eMudhra distrust update, even if the base image is cached.
RUN apk upgrade --no-cache && apk add --no-cache 'ca-certificates>=20261020-r0'

COPY bktest bktec /usr/local/bin/

ENTRYPOINT ["/usr/local/bin/bktec"]
