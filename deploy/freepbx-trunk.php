<?php
// Cria (ou confirma) um tronco PJSIP no FreePBX para um número do WaCalls.
// Uso: php freepbx-trunk.php <nome> <senha> [contexto]
// O nome do tronco É o usuário de autenticação (FreePBX força isso com
// autenticação inbound), por isso o WaCalls registra com esse mesmo nome.
if ($argc < 3) { fwrite(STDERR, "uso: freepbx-trunk.php <nome> <senha> [contexto]\n"); exit(2); }
$name = $argv[1]; $secret = $argv[2]; $context = $argv[3] ?? 'from-pstn';
if (!preg_match('/^[A-Za-z0-9][A-Za-z0-9_-]{1,31}$/', $name)) { fwrite(STDERR, "nome invalido\n"); exit(2); }

include '/etc/freepbx.conf';
$fpbx = FreePBX::Create();
$core = $fpbx->Core;
foreach ($core->listTrunks() as $t) {
    if (strcasecmp($t['name'], $name) === 0) { echo "tronco $name ja existe (id {$t['trunkid']})\n"; exit(0); }
}
$settings = [
    'channelid' => $name, 'trunk_name' => $name,
    'outcid' => '', 'keepcid' => 'off', 'maxchans' => '', 'failtrunk' => '', 'dialoutprefix' => '',
    'usercontext' => '', 'provider' => '', 'disabletrunk' => 'off', 'continue' => 'off',
    'sip_server' => '127.0.0.1', 'sip_server_port' => '5070',
    'username' => $name, 'auth_username' => '', 'secret' => $secret,
    'authentication' => 'inbound', 'registration' => 'receive',
    'context' => $context, 'transport' => '0.0.0.0-udp',
    'codec' => ['alaw' => 1, 'ulaw' => 2],
    'aor_contact' => '', 'from_domain' => '', 'from_user' => '', 'client_uri' => '', 'server_uri' => '',
    'contact_user' => '', 'outbound_proxy' => '',
    'qualify_frequency' => '60', 'dtmfmode' => 'rfc4733', 'language' => '',
    'sendrpid' => 'no', 'inband_progress' => 'no', 'direct_media' => 'no', 'rtp_symmetric' => 'yes',
    'rewrite_contact' => 'no', 'force_rport' => 'yes', 'support_path' => 'no',
    'media_address' => '', 'media_encryption' => 'no', 'message_context' => '',
    'identify_by' => 'auth_username',
    'expiration' => '3600', 'retry_interval' => '60', 'forbidden_retry_interval' => '30',
    'fatal_retry_interval' => '30', 'max_retries' => '10000',
    'auth_rejection_permanent' => 'off', 'allow_unauthenticated_options' => 'off',
    'trust_rpid' => 'no', 'trust_id_outbound' => 'no', 'send_connected_line' => 'yes',
    'user_eq_phone' => 'no', 'fax_detect' => 'no',
];
$id = $core->addTrunk($name, 'pjsip', $settings);
echo "tronco $name criado (id $id)\n";
