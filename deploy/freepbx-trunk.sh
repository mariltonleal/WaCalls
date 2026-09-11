#!/bin/bash
# Gancho do WaCalls (trunk_create_hook): cria o tronco no FreePBX e aplica.
# Args: <nome> <senha> <did>
set -e
NAME="$1"; SECRET="$2"
cd /var/www/html/admin
php /opt/wacalls/freepbx-trunk.php "$NAME" "$SECRET"
fwconsole reload >/dev/null 2>&1 && echo "configuracao aplicada"
