# syntax=docker/dockerfile:1.7
FROM mcr.microsoft.com/playwright:v1.55.0-noble
WORKDIR /tests
COPY package.json ./
# BuildKit RUN cache mounts: node_modules cache + the npm store are durable
# locally, so `npm install` is not re-downloaded every build. --pull=false
# (set in run.sh) also reuses the big playwright base image when we already
# have that tag locally.
RUN --mount=type=cache,target=/root/.npm npm install --ignore-scripts --no-audit --no-fund
COPY *.js ./
CMD ["npm", "test"]
