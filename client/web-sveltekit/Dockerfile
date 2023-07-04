# This image creates a standalone web server for demoing the prototype
FROM node:16@sha256:7755a15823204eb0b386505ff0f02c90f53b7bf19d902d0a1d7356af6c02b244

ARG PROJECT_ROOT="."

WORKDIR /usr/src/app
RUN echo '{"type": "module"}' > package.json
RUN npm install express@~4.18 http-proxy-middleware@~2.0
COPY  ${PROJECT_ROOT}/server.js ./
COPY ${PROJECT_ROOT}/build ./build

ENV SOURCEGRAPH_API_HOST=https://sourcegraph.com
EXPOSE 4173
CMD ["node", "server.js"]
