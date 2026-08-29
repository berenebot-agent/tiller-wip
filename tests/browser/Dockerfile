FROM mcr.microsoft.com/playwright:v1.55.0-noble
WORKDIR /tests
COPY package.json ./
RUN npm install --ignore-scripts --no-audit --no-fund
COPY playwright.config.js admin.spec.js ./
CMD ["npm", "test"]
