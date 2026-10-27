package remote

// Scripts run on the source with `sh -c`. They are POSIX sh (Ubuntu's dash, CentOS's
// bash), read-only except for the key removal, and print one record per line: a tag and
// tab-separated fields, cleaned of tabs and newlines on the source. They never print
// environment values or passwords: only names, sizes, counts and paths.

// lowPriority puts the script, and everything it starts, behind the source's own work
// (it still serves its site). Where that is not allowed nothing breaks.
const lowPriority = `renice -n 10 -p $$ >/dev/null 2>&1
ionice -c3 -p $$ >/dev/null 2>&1
`

// common is shared by the read-only scripts.
const common = `set -f
export LC_ALL=C
PATH=$PATH:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin
` + lowPriority + `T=$(printf '\t')
have() { command -v "$1" >/dev/null 2>&1; }
emit() {
	_l=$1; shift
	for _f in "$@"; do
		_f=$(printf '%s' "$_f" | tr '\t\r\n' '   ' | cut -c1-500)
		_l="$_l$T$_f"
	done
	printf '%s\n' "$_l"
}
lim() { if have timeout; then timeout "$@"; else shift; "$@"; fi; }
# count DIR: regular files, their bytes, folders, hidden entries, links, other entries
# (hidden ones are not descended into, like the copy leaves them out), and find's exit
# status: not 0 when a folder could not be read or the count ran out of time.
count() {
	{ lim 120 find "$1" -xdev -mindepth 1 \( \( -name '.*' -o -name __MACOSX \) -printf 'H %s\n' -prune \) -o -printf '%y %s\n' 2>/dev/null; echo "E $?"; } |
	awk '$1=="E"{e=$2;next} $1=="H"{h++;next} $1=="f"{f++;b+=$2;next} $1=="d"{d++;next} $1=="l"{l++;next} {o++} END{printf "%d %.0f %d %d %d %d %d\n", f, b, d, h, l, o, e}'
}
`

// mongoJS lists the databases with their sizes; it runs in the legacy mongo shell and
// in mongosh alike.
const mongoJS = `try{var r=db.adminCommand({listDatabases:1});if(!r.ok){print("FOOTOPS_MERR\t"+(r.errmsg||r.codeName))}else{print("FOOTOPS_MVER\t"+db.version());r.databases.forEach(function(d){var s=db.getSiblingDB(d.name).stats();print("FOOTOPS_MDB\t"+d.name+"\t"+Number(s.collections)+"\t"+Number(s.objects)+"\t"+Number(s.dataSize)+"\t"+Number(d.sizeOnDisk))})}}catch(e){print("FOOTOPS_MERR\t"+e)}`

// DiscoverScript looks around the source: system, tools, MongoDB, nginx, node
// processes, Docker containers and the folders apps keep uploads in.
const DiscoverScript = common + `JS='` + mongoJS + `'
os=$( . /etc/os-release 2>/dev/null && printf '%s' "$PRETTY_NAME" )
[ -n "$os" ] || os=$(cat /etc/redhat-release 2>/dev/null)
emit os "$os" "$(uname -r)" "$(uname -m)" "$(hostname 2>/dev/null)" "$(id -un)"
emit cpu "$(getconf _NPROCESSORS_ONLN 2>/dev/null)"
set -- $(awk '/^MemTotal:/{t=$2} /^MemAvailable:/{a=$2} END{print t+0, a+0}' /proc/meminfo 2>/dev/null)
emit mem "${1:-0}" "${2:-0}"
df -Pk / /home /var /srv /opt 2>/dev/null | awk 'NR>1 && !s[$6]++ {print $6, $2, $3, $4}' | while read -r m sz us av; do emit disk "$m" "$sz" "$us" "$av"; done

for t in mongodump mongorestore mongo mongosh mongod docker node pm2 nginx tar find rsync timeout; do
	have "$t" || continue
	case $t in
	mongodump|mongorestore|mongo|mongosh|mongod|tar|find|rsync) v=$("$t" --version 2>&1 | head -n 1) ;;
	docker) v=$(docker --version 2>&1) ;;
	node) v=$(node -v 2>&1) ;;
	nginx) v=$(nginx -v 2>&1) ;;
	*) v= ;;
	esac
	emit tool "$t" "$(command -v "$t")" "$v"
done

# mongosh keeps logs in $HOME: a throwaway one, so the source is left as it was.
mongo_list() {
	h=$(mktemp -d 2>/dev/null) || h=
	out=$( [ -z "$h" ] || export HOME="$h"; lim 20 "$@" --quiet --norc --eval "$JS" 2>&1)
	[ -z "$h" ] || rm -rf "$h"
	printf '%s\n' "$out" | grep '^FOOTOPS_M' | head -n 300
	printf '%s\n' "$out" | grep -q '^FOOTOPS_M' || emit merr "$(printf '%s' "$out" | tail -n 2)"
}
if have mongosh; then emit msrc host - mongosh; mongo_list mongosh
elif have mongo; then emit msrc host - mongo; mongo_list mongo
fi

cwds=
set +f
procs=$(echo /proc/[0-9]*)
set -f
for p in $procs; do
	exe=$(readlink "$p/exe" 2>/dev/null) || continue
	case $exe in */node|*/nodejs|*'/node (deleted)') ;; *) continue ;; esac
	cmd=$(tr '\0' ' ' < "$p/cmdline" 2>/dev/null)
	case $cmd in "PM2 v"*) ;; *) set -- $cmd; cmd="${1:-} ${2:-}" ;; esac
	cwd=$(readlink "$p/cwd" 2>/dev/null)
	env=$(tr '\0' '\n' < "$p/environ" 2>/dev/null | grep -E '^(name|pm_id|PM2_HOME|PORT|NODE_ENV)=' | head -n 5 | tr '\n' '|')
	emit proc "${p#/proc/}" "$(awk '/^PPid:/{print $2}' "$p/status" 2>/dev/null)" "$(stat -c %U "$p" 2>/dev/null)" \
		"$(awk '/^VmRSS:/{print $2}' "$p/status" 2>/dev/null)" "$exe" "$cwd" "$cmd" "$env"
	cwds="$cwds$cwd
"
done
if have ss; then lim 10 ss -ltnp 2>/dev/null | sed 1d | head -n 300 | tr '\t' ' ' | sed "s/^/listen$T/"; fi

if have docker; then
	lim 20 docker ps -a --format '{{.Names}}{{"\t"}}{{.Image}}{{"\t"}}{{.Status}}{{"\t"}}{{.Ports}}' 2>/dev/null | head -n 100 | tr '\r' ' ' | sed "s/^/ctr$T/"
	for id in $(lim 20 docker ps -q 2>/dev/null | head -n 100); do
		lim 10 docker inspect --format '{{.Name}}{{range .Mounts}}{{"\t"}}{{.Source}}:{{.Destination}}{{end}}' "$id" 2>/dev/null | head -n 1 | sed "s/^\//mnt$T/"
	done
	lim 20 docker ps --format '{{.Names}}{{"\t"}}{{.Image}}' 2>/dev/null | head -n 100 | while IFS="$T" read -r n img; do
		case $img in *mongo*) ;; *) continue ;; esac
		for s in mongosh mongo; do
			lim 10 docker exec "$n" sh -c "command -v $s" >/dev/null 2>&1 || continue
			emit msrc docker "$n" "$s"
			mongo_list docker exec "$n" "$s"
			break
		done
	done
fi

if have nginx; then lim 20 nginx -T 2>/dev/null | head -c 1000000 | tr '\t\r' '  ' | sed "s/^/ngx$T/"; fi

printf '%s' "$cwds" | sort -u | head -n 30 | while IFS= read -r d; do
	[ -n "$d" ] && [ -d "$d" ] && [ "$d" != / ] || continue
	emit app "$d" "$(lim 60 du -sk "$d" 2>/dev/null | cut -f1)"
	for s in public/uploads uploads public/upload upload data storage files media public/images public/files static/uploads; do
		c=$d/$s
		[ -d "$c" ] || continue
		# An uploads folder that links elsewhere is copied from where it points.
		[ -L "$c" ] && c=$(readlink -f "$c")
		emit cand "$c" $(count "$c")
	done
done
printf 'end\n'
`

// ScanScript looks into an app folder: what is in it, what kind of app it is, which
// secrets sit in its code (file and line only) and which addresses its front end has
// built in. $1 is the folder.
const ScanScript = common + `d=$1
if [ ! -d "$d" ]; then emit err "klasör yok: $d"; printf 'end\n'; exit 0; fi
cd -- "$d" 2>/dev/null || { emit err "klasöre girilemedi: $d"; printf 'end\n'; exit 0; }
emit dir "$(pwd -P)"
emit total $(count .)
lim 30 find . -xdev -mindepth 1 -maxdepth 1 -type d ! -name '.*' 2>/dev/null | sort | head -n 60 | while IFS= read -r s; do
	emit sub "${s#./}" $(count "$s")
done
for f in package.json Dockerfile docker-compose.yml docker-compose.yaml compose.yaml compose.yml ecosystem.config.js .nvmrc next.config.js next.config.mjs server.js app.js index.js; do
	[ -f "$f" ] && emit file "$f"
done
if [ -f package.json ]; then
	awk '
	/^  "name": "/ { v=$0; sub(/^  "name": "/, "", v); sub(/".*$/, "", v); print "pkgname\t" v }
	/^  "main": "/ { v=$0; sub(/^  "main": "/, "", v); sub(/".*$/, "", v); print "pkgmain\t" v }
	/^    "node": "/ { v=$0; sub(/^    "node": "/, "", v); sub(/".*$/, "", v); print "pkgnode\t" v }
	/^  "(dependencies|devDependencies)": \{/ { dep=1; next }
	dep && /^  }/ { dep=0 }
	dep && /^    "/ { v=$0; sub(/^    "/, "", v); sub(/".*$/, "", v); print "dep\t" v }
	' package.json 2>/dev/null | head -n 300 | tr -d '\r'
fi
# Variable names only. A quoted value may run over several lines (a key in PEM form):
# those lines are skipped, so no piece of a value is ever taken for a name.
for f in .env .env.local .env.production .env.development; do
	[ -f "$f" ] || continue
	emit env "$f" "$(awk '
		q != "" { if (index($0, q)) q = ""; next }
		/^[ \t]*(export[ \t]+)?[A-Za-z_][A-Za-z0-9_]*[ \t]*=/ {
			l = $0; sub(/^[ \t]*(export[ \t]+)?/, "", l)
			n = l; sub(/[ \t]*=.*$/, "", n)
			v = l; sub(/^[^=]*=[ \t]*/, "", v)
			c = substr(v, 1, 1)
			if ((c == "\"" || c == "'"'"'") && index(substr(v, 2), c) == 0) q = c
			if (length(n) <= 64) print n
		}' "$f" 2>/dev/null | head -n 100 | tr '\n' ' ')"
done
G="--exclude-dir=node_modules --exclude-dir=.git --exclude-dir=.next --exclude-dir=.cache --exclude-dir=bower_components"
secret() {
	lim 60 grep -rnIE $G -e "$2" . 2>/dev/null | head -n 40 | cut -d: -f1,2 | while IFS= read -r m; do emit secret "$1" "${m#./}"; done
}
secret mongo-uri 'mongodb(\+srv)?://[^/@[:space:]]+:[^/@[:space:]]+@'
secret password "(pass|passwd|password|parola|sifre)[\"']?[[:space:]]*[:=][[:space:]]*[\"'][^\"'[:space:]]{3,}"
secret api-key "(api[_-]?key|secret|token|access[_-]?key)[\"']?[[:space:]]*[:=][[:space:]]*[\"'][A-Za-z0-9_./+=-]{16,}"
secret private-key '-----BEGIN [A-Z ]*PRIVATE KEY-----'
secret smtp '(smtp\.[a-z0-9.-]+|createTransport\()'
lim 60 grep -rhoIE $G 'mongodb(\+srv)?://[^[:space:]"'"'"'` + "`" + `]+' . 2>/dev/null | sed -nE 's#^mongodb(\+srv)?://[^/]*/([A-Za-z0-9_-]+).*$#\2#p' | sort | uniq -c | sort -rn | head -n 20 |
	while read -r n db; do emit dbref "$db" "$n"; done
lim 60 grep -rhoIE $G 'https?://(([0-9]{1,3}\.){3}[0-9]{1,3}|localhost)(:[0-9]+)?' . 2>/dev/null | sort | uniq -c | sort -rn | head -n 30 |
	while read -r n u; do emit url "$u" "$n"; done
printf 'end\n'
`

// CountScript counts a folder like the copy takes it. $1 is the folder.
const CountScript = common + `cd -- "$1" 2>/dev/null || exit 3
printf 'FOOTOPS_COUNT %s\n' "$(count .)"
`

// DumpScript streams a database as a gzip'd mongodump archive to stdout: $1 "host" or
// "docker", $2 the container, $3 the database. The tool's progress, with the documents
// per collection, goes to stderr.
const DumpScript = `export LC_ALL=C
PATH=$PATH:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin
` + lowPriority + `db=$3
case $1 in
host)
	command -v mongodump >/dev/null 2>&1 || exit 69
	exec mongodump --archive --gzip --db="$db" ;;
docker) exec docker exec "$2" mongodump --archive --gzip --db="$db" ;;
esac
exit 64
`

// TarScript streams a folder as an uncompressed tar to stdout: regular files and
// folders only (no links, devices or sockets; hard links as plain files), hidden entries
// left out, other file systems not entered. $1 is the folder. GNU find and tar are
// required. A failing find says so on stderr (its status is lost in the pipe), and the
// stream ends with "FOOTOPS_TAR_END <tar's status>" after the archive: a stream without
// it was cut off, however complete the tar in it looks.
const TarScript = `export LC_ALL=C
PATH=$PATH:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin
` + lowPriority + `find --version 2>/dev/null | grep -q GNU || exit 66
tar --version 2>/dev/null | grep -q GNU || exit 66
cd -- "$1" 2>/dev/null || exit 3
{ find . -xdev -mindepth 1 \( -name '.*' -o -name __MACOSX \) -prune -o \( -type f -o -type d \) -print0; s=$?; [ "$s" = 0 ] || echo "find: exit status $s" >&2; } |
	tar --null --no-recursion --hard-dereference -T - -cf -
s=$?
printf '` + TarEnd + ` %d\n' "$s"
exit "$s"
`

// TarEnd starts the line TarScript writes after the archive.
const TarEnd = "FOOTOPS_TAR_END"
