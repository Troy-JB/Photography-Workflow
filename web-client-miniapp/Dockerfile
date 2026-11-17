FROM node:24-alpine AS build

WORKDIR /app
COPY package.json package-lock.json ./
RUN npm ci
COPY . .
ARG VITE_GALLERY_BASE_URL=http://localhost:5174
ENV VITE_GALLERY_BASE_URL=$VITE_GALLERY_BASE_URL
RUN npm run build:h5

FROM nginx:1.27-alpine
COPY nginx.conf /etc/nginx/conf.d/default.conf
COPY --from=build /app/dist/build/h5/ /usr/share/nginx/html/
EXPOSE 80
