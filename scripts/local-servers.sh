#!/usr/bin/env sh
# Start PostgreSQL and MySQL for the integration suite without Docker.
#
#   . scripts/local-servers.sh          # start, and export the DSNs
#   scripts/local-servers.sh stop       # stop
#
# docker-compose.yml is the supported path and the one CI mirrors. This
# script exists because a container without a Docker daemon is a normal
# place to work, and the alternative there is running no integration
# tests at all.
#
# It covers PostgreSQL and MySQL/MariaDB only. ClickHouse and Qdrant
# ship no distribution packages worth depending on; leave their DSNs
# unset and those tests skip.
#
# Debian and Ubuntu. Install first:
#
#   apt-get install -y postgresql-16 postgresql-16-pgvector \
#                      postgresql-16-postgis-3 mariadb-server

set -e

PGBIN=${PGBIN:-/usr/lib/postgresql/16/bin}
PGDATA_IT=${PGDATA_IT:-/var/lib/postgresql/drops-it}
MYDATA_IT=${MYDATA_IT:-/var/lib/mysql-drops-it}
PGPORT_IT=${PGPORT_IT:-5433}
MYPORT_IT=${MYPORT_IT:-3307}

# The same offset ports as docker-compose.yml, for the same reason: a
# suite pointed at the wrong DSN should fail to connect rather than
# quietly write into a database you care about.

stop() {
	if [ -d "$PGDATA_IT" ]; then
		su postgres -c "$PGBIN/pg_ctl -D $PGDATA_IT stop -m fast" || true
	fi
	if [ -f /var/run/mysqld/drops-it.pid ]; then
		kill "$(cat /var/run/mysqld/drops-it.pid)" || true
	fi
}

if [ "$1" = stop ]; then
	stop
	exit 0
fi

if [ ! -s "$PGDATA_IT/PG_VERSION" ]; then
	mkdir -p "$PGDATA_IT"
	chown postgres:postgres "$PGDATA_IT"
	su postgres -c "$PGBIN/initdb -D $PGDATA_IT -U drops --auth=trust"
fi
su postgres -c "$PGBIN/pg_ctl -D $PGDATA_IT -l $PGDATA_IT/log \
	-o '-p $PGPORT_IT -c listen_addresses=127.0.0.1 -c unix_socket_directories=/tmp' \
	-w start" || true

psql "postgres://drops@127.0.0.1:$PGPORT_IT/postgres" \
	-tc "select 1 from pg_database where datname='drops'" | grep -q 1 ||
	psql "postgres://drops@127.0.0.1:$PGPORT_IT/postgres" -c "create database drops"

# pgvector and PostGIS are what make the vector-store and geometry tests
# run rather than skip, which is where the risk is highest. Missing is
# not fatal — those tests skip on the extension's absence.
psql "postgres://drops@127.0.0.1:$PGPORT_IT/drops" \
	-c "create extension if not exists vector" \
	-c "create extension if not exists postgis" || true

if [ ! -d "$MYDATA_IT/mysql" ]; then
	mkdir -p "$MYDATA_IT" /var/run/mysqld
	chown -R mysql:mysql "$MYDATA_IT" /var/run/mysqld
	mariadb-install-db --user=mysql --datadir="$MYDATA_IT" \
		--auth-root-authentication-method=normal >/dev/null
fi
if ! mariadb -h 127.0.0.1 -P "$MYPORT_IT" -u root -e 'select 1' >/dev/null 2>&1; then
	mariadbd --user=mysql --datadir="$MYDATA_IT" --port="$MYPORT_IT" \
		--socket=/var/run/mysqld/drops-it.sock --bind-address=127.0.0.1 \
		--pid-file=/var/run/mysqld/drops-it.pid >/tmp/drops-mariadb.log 2>&1 &
	i=0
	while [ $i -lt 40 ]; do
		mariadb -h 127.0.0.1 -P "$MYPORT_IT" -u root -e 'select 1' >/dev/null 2>&1 && break
		i=$((i + 1))
		sleep 1
	done
fi

# The anonymous ''@'localhost' account matches before drops@'%' does, so
# a connection over 127.0.0.1 authenticates as nobody and is refused.
mariadb -h 127.0.0.1 -P "$MYPORT_IT" -u root -e "
	delete from mysql.global_priv where user='';
	create user if not exists 'drops'@'localhost' identified by 'drops';
	create user if not exists 'drops'@'127.0.0.1' identified by 'drops';
	create user if not exists 'drops'@'%' identified by 'drops';
	grant all on *.* to 'drops'@'localhost';
	grant all on *.* to 'drops'@'127.0.0.1';
	grant all on *.* to 'drops'@'%';
	create database if not exists drops;
	flush privileges;"

DROPS_PG_DSN="postgres://drops@127.0.0.1:$PGPORT_IT/drops?sslmode=disable"
DROPS_MYSQL_DSN="drops:drops@tcp(127.0.0.1:$MYPORT_IT)/drops?parseTime=true"
export DROPS_PG_DSN DROPS_MYSQL_DSN

echo "DROPS_PG_DSN=$DROPS_PG_DSN"
echo "DROPS_MYSQL_DSN=$DROPS_MYSQL_DSN"
