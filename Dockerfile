# The dispatcher image: factory (the dispatcher and CLI) with the web UI embedded
# (.github/workflows/release-image.yml pushes it on a published release; make image builds it
# locally; deploy/poc/README.md §Run the image). Every base is pinned by its multi-arch index
# digest, as a tag can be repointed; refresh one with `docker buildx imagetools inspect <tag>`.
# The build stages run on the build machine's platform and cross-compile, so a multi-platform
# build emulates nothing. Nothing secret is in the image: flags and FACTORY_* come at run time.

# The web UI: the same npm ci && npm run build as make web, into internal/webui/dist.
FROM --platform=$BUILDPLATFORM docker.io/library/node:20.20.2-alpine3.23@sha256:fb4cd12c85ee03686f6af5362a0b0d56d50c58a04632e6c0fb8363f609372293 AS web
WORKDIR /src/web
COPY web/package.json web/package-lock.json ./
RUN npm ci
COPY web/ ./
# The MSW mocks and the type check read the contract's examples.
COPY api/ /src/api/
RUN npm run build

# factory, static, with the UI embedded (-tags webui), as make build-release builds it.
FROM --platform=$BUILDPLATFORM docker.io/library/golang:1.26.10-alpine3.23@sha256:34414731924cf2263160f0021f5cf6e1807382d85248f7cb419d4187b96d5e43 AS build
ARG TARGETOS TARGETARCH
ARG VERSION=dev
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
COPY --from=web /src/internal/webui/dist/ internal/webui/dist/
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -tags webui -trimpath \
	-ldflags "-X github.com/ContainerSolutions/factory/internal/version.Version=${VERSION}" \
	-o /out/factory ./cmd/factory

# The runtime: git (the catalog's git source and the skills refs exec it), the CA roots and an
# SSH client (an ssh --catalog-repo read with a deploy key through GIT_SSH_COMMAND,
# deploy/poc/README.md §4), as a non-root user. GitHub's host keys are pinned from
# deploy/github_known_hosts, so nothing is fetched at build time. /var/lib/factory is for the
# state dir (--state-dir /var/lib/factory/state).
FROM docker.io/library/alpine:3.23@sha256:ff7f0c727dd796aa36787bfcd9a64bf35a6ce72a73451b3359fde849b6703d63
RUN apk add --no-cache git ca-certificates openssh-client-default \
	&& addgroup -S -g 65532 factory \
	&& adduser -S -D -H -u 65532 -G factory -h /var/lib/factory factory \
	&& install -d -m 0700 -o factory -g factory /var/lib/factory
COPY --chmod=0644 deploy/github_known_hosts /etc/ssh/ssh_known_hosts
COPY --from=build /out/factory /usr/local/bin/factory
USER 65532:65532
ENV HOME=/var/lib/factory
WORKDIR /var/lib/factory
VOLUME ["/var/lib/factory"]
# The public listener (factory serve --addr, default :8800). The ops listener stays on loopback.
EXPOSE 8800
ENTRYPOINT ["factory"]
CMD ["serve"]
