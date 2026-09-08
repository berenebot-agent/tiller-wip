# syntax=docker/dockerfile:1.7
FROM mcr.microsoft.com/playwright:v1.56.0-noble
WORKDIR /tests
# Lockfile-driven install: package.json + package-lock.json are copied first so
# Docker's layer cache invalidates only when deps actually change. npm ci is
# deterministic and faster than npm install on a cold cache. The BuildKit npm
# store cache (--mount below) makes repeated local builds cheap; --pull=false
# (set in run.sh) also reuses the big playwright base image locally.
COPY package.json package-lock.json ./
RUN --mount=type=cache,target=/root/.npm npm ci --ignore-scripts --no-audit --no-fund
COPY *.js ./
CMD ["npm", "test"]
