# TLS certificates

`nginx.conf` expects `server.crt` and `server.key` in this directory. They
are intentionally not committed.

For local development, generate a self-signed pair:

```sh
openssl req -x509 -nodes -newkey rsa:2048 \
  -keyout docker/nginx/certs/server.key \
  -out docker/nginx/certs/server.crt \
  -days 365 \
  -subj "/CN=localhost"
```

For a real deployment, replace these with certificates from your CA (e.g.
Let's Encrypt via a separate ACME client, or certificates issued by an
internal CA) and mount them the same way.
