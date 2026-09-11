# WaCalls (fork) — WhatsApp voz como tronco SIP do FreePBX

Fork de `JotaDev66/WaCalls` (Go, whatsmeow + codec MLow puro Go). O que este
fork acrescenta é o **modo tronco SIP** (`internal/siptrunk`): cada número de
WhatsApp pareado registra no FreePBX como tronco PJSIP; ligações fluem nos dois
sentidos com áudio G.711. Documentação completa em `docs/TRONCO-SIP.md`
(leia antes de mexer).

## Onde roda

- **Produção:** VPS `root@187.127.36.66` (pbx.krato.ai, Debian 12, FreePBX 17,
  Asterisk 22). Serviço `wacalls` (systemd), binário `/opt/wacalls/wacalls`,
  config `/opt/wacalls/trunks.json`, fonte em `/opt/wacalls-src`.
- **Painel:** https://pbx.krato.ai/wacalls/ — usuário `krato`, senha
  `WACALLS_PANEL_PASS` no cofre `~\.claude\skills\.env` (senha própria, NÃO é
  a única dos painéis).
- **GitHub:** `mariltonleal/WaCalls` (origin, branch `main`); upstream
  `JotaDev66/WaCalls`.

## Regras de trabalho

- **Compilar no VPS, não no PC:** o link do binário estoura a memória do PC.
  Enviar fonte com `tar | ssh` (comando em `docs/TRONCO-SIP.md`) e rodar
  `go build` lá (`/usr/local/go/bin/go`). Depois `systemctl restart wacalls`.
- Testes: `go test ./internal/siptrunk ./cmd/server` (no VPS).
- Segredos (senhas SIP) ficam só em `/opt/wacalls/trunks.json` no VPS. Nunca
  commitar `trunks.json` nem senhas.
- O repo tem CRLF no working tree (autocrlf); `gofmt -l` lista tudo por isso.
  Ignorar; não converter arquivos em massa.
- Sessões não pareadas somem quando o serviço reinicia (comportamento do
  upstream). Para reparear: painel → "Criar sessão e gerar QR".
- Nome do tronco no FreePBX = `username` do trunks.json = string de discagem
  (`channelid`). Os três têm que bater.
- Antes de parear número de produção aqui, desligar o WaCalls do hub
  (72.60.137.231, `/opt/wacalls`): o mesmo número com dois WaCalls disputa as
  chamadas.

## Layout

- `internal/siptrunk/config.go` — trunks.json, defaults, Add/Remove/Reload.
- `internal/siptrunk/trunk.go` — registro no PABX, INVITE nos dois sentidos,
  mapeamento de motivos → códigos SIP.
- `internal/siptrunk/leg.go` — uma chamada bridgeada: bombas de áudio.
- `internal/siptrunk/resample.go` — 16 kHz ↔ 8 kHz.
- `cmd/server/session.go` — ganchos do tronco na sessão; `DialWhatsApp`.
- `cmd/server/httpapi.go` — `/api/trunks`.
- `web/index.html` — painel (sessões, QR ao vivo, novo número).
- `deploy/` — script PHP/sh que cria o tronco no FreePBX; conf do Apache.

## Estado (11/09/2026)

Número de teste 5519987651060 pareado (sessão `teste`, tronco `WA-Teste`).
Entrada (→ ramal 1001) e saída (`*3` + número) testadas com áudio OK.
Pendente: parear Tubras e Verdepack (após desligar o WaCalls do hub),
firewall/hardening do VPS, opcional codec Opus.
