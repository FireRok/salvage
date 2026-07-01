FROM postgres:16
RUN apt-get update \
 && apt-get install -y --no-install-recommends pgbackrest \
 && rm -rf /var/lib/apt/lists/*
COPY pgbackrest.conf /etc/pgbackrest/pgbackrest.conf
RUN mkdir -p /var/lib/pgbackrest /var/log/pgbackrest \
 && chown -R postgres:postgres /var/lib/pgbackrest /var/log/pgbackrest /etc/pgbackrest
