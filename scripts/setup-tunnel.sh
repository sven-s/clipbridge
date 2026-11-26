#!/bin/bash
# Guided Cloudflare Tunnel setup for Clipbridge.
#
# Idempotent: every step checks what already exists and only does what is
# missing, so re-running it is how you repair a broken setup. Existing files
# are never overwritten without asking, and a .bak copy is kept.
#
# Launched from the menu bar (Tunnel → Set Up Cloudflare Tunnel…) or by hand:
#   bash scripts/setup-tunnel.sh
#
# Written for the bash 3.2 that ships with macOS. No jq: JSON goes through plutil.

set -euo pipefail

CB_CONFIG="$HOME/.clipbridge/config.json"
CF_DIR="$HOME/.cloudflared"
CF_CONFIG="$CF_DIR/config.yml"
CF_LOG="$CF_DIR/clipbridge-tunnel.log"
AGENTS="$HOME/Library/LaunchAgents"
DEFAULT_LABEL="de.sven.clipbridge-tunnel"

bold() { printf '\n\033[1m==> %s\033[0m\n' "$*"; }
ok()   { printf '    \033[32m✓\033[0m %s\n' "$*"; }
warn() { printf '    \033[33m!\033[0m %s\n' "$*"; }
die()  { printf '\n\033[31m✗ %s\033[0m\n' "$*" >&2; exit 1; }

ask() { # ask "prompt" default → echoes answer
	local reply
	read -r -p "    $1 [$2]: " reply
	echo "${reply:-$2}"
}

confirm() { # confirm "question" → 0 on yes
	local reply
	read -r -p "    $1 [y/N]: " reply
	[[ "$reply" =~ ^[Yy] ]]
}

json_get() { # json_get file key → value or empty
	[ -f "$1" ] && plutil -extract "$2" raw -o - "$1" 2>/dev/null || true
}

# json_set file key value: set one string key, keep the file pretty-printed
# (plutil -replace works but rewrites JSON minified with escaped slashes).
json_set() {
	osascript -l JavaScript - "$1" "$2" "$3" <<'JS'
ObjC.import('Foundation');
function run(argv) {
	var path = argv[0], enc = $.NSUTF8StringEncoding;
	var obj = JSON.parse($.NSString.stringWithContentsOfFileEncodingError(path, enc, null).js);
	obj[argv[1]] = argv[2];
	var out = $.NSString.alloc.initWithUTF8String(JSON.stringify(obj, null, 2) + "\n");
	if (!out.writeToFileAtomicallyEncodingError(path, true, enc, null)) throw new Error("write failed: " + path);
}
JS
}

# write_file path content: create, or show a diff and ask before replacing.
write_file() {
	local path="$1" content="$2"
	if [ -f "$path" ]; then
		if [ "$(cat "$path")" = "$content" ]; then
			ok "$path already up to date"
			return 0
		fi
		warn "$path exists and differs:"
		diff -u "$path" <(printf '%s\n' "$content") | sed 's/^/      /' || true
		if ! confirm "Replace it (old copy kept as .bak)?"; then
			warn "kept existing $path"
			return 1
		fi
		cp "$path" "$path.bak"
	fi
	mkdir -p "$(dirname "$path")"
	printf '%s\n' "$content" > "$path"
	ok "wrote $path"
}

echo
echo "Clipbridge — Cloudflare Tunnel setup"
echo "Requires a domain whose nameservers point at Cloudflare."

# --- 1. cloudflared -----------------------------------------------------------
bold "cloudflared"
CF_BIN="$(command -v cloudflared || true)"
for p in /opt/homebrew/bin/cloudflared /usr/local/bin/cloudflared; do
	[ -z "$CF_BIN" ] && [ -x "$p" ] && CF_BIN="$p"
done
if [ -z "$CF_BIN" ]; then
	command -v brew >/dev/null || die "Homebrew not found. Install cloudflared manually, then re-run."
	confirm "cloudflared is missing. Install it with Homebrew?" || die "cloudflared is required."
	brew install cloudflared
	CF_BIN="$(command -v cloudflared)"
fi
ok "$CF_BIN ($("$CF_BIN" --version 2>/dev/null | head -1))"

# --- 2. Hostname + port -------------------------------------------------------
bold "Hostname"
current_url="$(json_get "$CB_CONFIG" public_url)"
current_host="${current_url#https://}"; current_host="${current_host#http://}"; current_host="${current_host%/}"
HOST="$(ask "Public hostname (e.g. clip.example.com)" "${current_host:-}")"
[ -n "$HOST" ] || die "A hostname is required."
PORT="$(json_get "$CB_CONFIG" server_port)"; PORT="${PORT:-8457}"
TUNNEL_NAME="$(ask "Tunnel name" "clipbridge")"
ok "https://$HOST → http://127.0.0.1:$PORT via tunnel '$TUNNEL_NAME'"

# --- 3. Login -----------------------------------------------------------------
bold "Cloudflare login"
if [ -f "$CF_DIR/cert.pem" ]; then
	ok "already logged in ($CF_DIR/cert.pem)"
else
	echo "    A browser opens — pick the zone that holds $HOST."
	"$CF_BIN" tunnel login
fi

# --- 4. Tunnel ----------------------------------------------------------------
bold "Tunnel"
tunnel_id() {
	"$CF_BIN" tunnel list --name "$TUNNEL_NAME" -o json 2>/dev/null |
		plutil -extract 0.id raw -o - - 2>/dev/null || true
}
UUID="$(tunnel_id)"
if [ -z "$UUID" ]; then
	"$CF_BIN" tunnel create "$TUNNEL_NAME"
	UUID="$(tunnel_id)"
	[ -n "$UUID" ] || die "Tunnel creation did not produce an id."
	ok "created $TUNNEL_NAME ($UUID)"
else
	ok "exists: $TUNNEL_NAME ($UUID)"
fi
CREDS="$CF_DIR/$UUID.json"
if [ ! -f "$CREDS" ]; then
	die "Tunnel '$TUNNEL_NAME' exists but $CREDS is missing (created on another machine?).
  Either copy that file here, or delete the tunnel in the Cloudflare dashboard
  ('cloudflared tunnel delete $TUNNEL_NAME') and re-run this script."
fi

# --- 5. DNS -------------------------------------------------------------------
bold "DNS"
if out="$("$CF_BIN" tunnel route dns "$TUNNEL_NAME" "$HOST" 2>&1)"; then
	ok "$HOST → $TUNNEL_NAME"
else
	# Most common cause: the record already exists (from an earlier run, or
	# pointing at an old tunnel). Only the dashboard knows which.
	warn "route dns said: $(echo "$out" | tail -1)"
	warn "If $HOST already points at this tunnel, that's fine. Otherwise fix the CNAME in the Cloudflare dashboard."
fi

# --- 6. config.yml ------------------------------------------------------------
bold "cloudflared config"
CF_YAML="tunnel: $UUID
credentials-file: $CREDS

originRequest:
  connectTimeout: 30s
  noHappyEyeballs: true

ingress:
  - hostname: $HOST
    service: http://127.0.0.1:$PORT
  - service: http_status:404"
write_file "$CF_CONFIG" "$CF_YAML" || true
"$CF_BIN" --config "$CF_CONFIG" tunnel ingress validate >/dev/null || die "cloudflared rejects $CF_CONFIG."
ok "ingress rules valid"

# --- 7. LaunchAgent -----------------------------------------------------------
bold "LaunchAgent"
existing=""
for f in "$AGENTS"/*clipbridge-tunnel.plist; do
	[ -f "$f" ] && existing="$f" && break
done
if [ -n "$existing" ]; then
	PLIST="$existing"
	LABEL="$(plutil -extract Label raw -o - "$PLIST" 2>/dev/null || echo "$DEFAULT_LABEL")"
	ok "reusing $PLIST ($LABEL)"
else
	LABEL="$DEFAULT_LABEL"
	PLIST="$AGENTS/$LABEL.plist"
fi
PLIST_XML="<?xml version=\"1.0\" encoding=\"UTF-8\"?>
<!DOCTYPE plist PUBLIC \"-//Apple//DTD PLIST 1.0//EN\" \"http://www.apple.com/DTDs/PropertyList-1.0.dtd\">
<plist version=\"1.0\">
<dict>
    <key>Label</key>
    <string>$LABEL</string>
    <key>ProgramArguments</key>
    <array>
        <string>$CF_BIN</string>
        <string>--config</string>
        <string>$CF_CONFIG</string>
        <string>--no-autoupdate</string>
        <string>tunnel</string>
        <string>run</string>
        <string>$TUNNEL_NAME</string>
    </array>
    <key>RunAtLoad</key>
    <true/>
    <key>KeepAlive</key>
    <true/>
    <key>StandardOutPath</key>
    <string>$CF_LOG</string>
    <key>StandardErrorPath</key>
    <string>$CF_LOG</string>
    <key>WorkingDirectory</key>
    <string>$HOME</string>
</dict>
</plist>"
write_file "$PLIST" "$PLIST_XML" || true
plutil -lint "$PLIST" >/dev/null || die "$PLIST is not a valid plist."

TARGET="gui/$(id -u)/$LABEL"
# bootout + bootstrap rather than kickstart: picks up plist edits, too.
launchctl bootout "$TARGET" 2>/dev/null || true
launchctl bootstrap "gui/$(id -u)" "$PLIST"
ok "loaded $LABEL"

# --- 8. Clipbridge config -----------------------------------------------------
bold "Clipbridge config"
URL="https://$HOST"
if [ ! -f "$CB_CONFIG" ]; then
	warn "$CB_CONFIG not found — start Clipbridge once so it generates a secret, then re-run."
elif [ "$current_url" = "$URL" ]; then
	ok "public_url already $URL"
else
	cp "$CB_CONFIG" "$CB_CONFIG.bak"
	json_set "$CB_CONFIG" public_url "$URL"
	chmod 600 "$CB_CONFIG"
	ok "public_url = $URL (old copy in config.json.bak)"
	NEED_RESTART=1
fi

# --- 9. Check -----------------------------------------------------------------
bold "End-to-end check"
for i in 1 2 3 4 5 6; do
	if body="$(curl -fsS --max-time 10 "$URL/healthz" 2>/dev/null)" && [ "$body" = "clipbridge ok" ]; then
		ok "$URL/healthz answers — tunnel is live"
		break
	fi
	[ "$i" = 6 ] && warn "$URL/healthz not answering yet. Check the log: $CF_LOG (DNS can take a minute; is Clipbridge running?)"
	sleep 5
done

echo
if [ "${NEED_RESTART:-0}" = 1 ]; then
	echo "Done. In the menu bar: Tunnel → Restart Clipbridge, so the new public_url is used."
else
	echo "Done."
fi
