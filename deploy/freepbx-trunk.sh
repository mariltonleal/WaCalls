#!/bin/bash
# Gancho do WaCalls (trunk_create_hook): cria o tronco no FreePBX e aplica.
# Args: <nome> <senha> <did>
# Saída vai para o painel; por isso os erros do fwconsole são mostrados.
NAME="$1"; SECRET="$2"
cd /var/www/html/admin || exit 1
php /opt/wacalls/freepbx-trunk.php "$NAME" "$SECRET" || exit 1

# "fwconsole reload" falha se outro reload estiver em andamento (ex.: alguém
# clicou em "Apply Config" na mesma hora). Tenta algumas vezes.
for i in 1 2 3 4 5; do
  OUT=$(fwconsole reload 2>&1)
  RC=$?
  if [ $RC -eq 0 ]; then
    echo "configuracao aplicada"
    exit 0
  fi
  echo "fwconsole reload falhou (tentativa $i, codigo $RC): $(echo "$OUT" | tail -2 | tr '\n' ' ')"
  sleep 6
done
echo "ATENCAO: o tronco foi criado mas o reload nao aplicou; rode 'fwconsole reload' no VPS ou clique em Apply Config."
exit 1
