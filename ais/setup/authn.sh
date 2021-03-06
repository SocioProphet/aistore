cat > $CONFFILE <<EOL
{
	"confdir": "$CONFDIR",
	"log": {
		"dir":   "$LOGDIR",
		"level": "${LOGLEVEL:-3}"
	},
	"proxy": {
		"url": "$PROXYURL"
	},
	"net": {
		"http": {
			"port": ${AUTHN_PORT:-52001},
			"use_https": false,
			"server_cert": "server.pem",
			"server_key": "server.key"
		}
	},
	"auth": {
		"secret": "$SECRETKEY",
		"username": "${AUTHN_SU_NAME:-admin}",
		"password": "${AUTHN_SU_PASS:-admin}",
		"expiration_time": "${AUTHN_TTL:-24h}"
	},
	"timeout": {
		"default_timeout": "30s"
	}
}
EOL

