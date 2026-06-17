```shell
docker compose build
docker save raydio:local raydio-worker:local raydio-suno-worker:local | gzip > images.tar.gz
# rsync -avz images.tar.gz user@your-vps:/tmp/

# ssh ...
docker load < images.tar.gz
docker compose up -d
```
