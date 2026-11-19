#!/usr/bin/env bash
# Tests databases changed by narcissus against the SQLite versions supported
# operating systems ship (docs/safety.md, "Supported SQLite versions"), each playing the app.
set -euo pipefail

VERSIONS="2018:3260000 2021:3340100 2022:3370200 2022:3400100 2024:3450100 2024:3460100"
cache=${XDG_CACHE_HOME:-$HOME/.cache}/narcissus-compat
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
root=$(cd "$(dirname "$0")/.." && pwd)
go build -o "$work/narcissus" "$root/cmd/narcissus"
N=$work/narcissus

mkdir -p "$cache"
for v in $VERSIONS; do
  year=${v%%:*} num=${v#*:}
  [ -x "$cache/sqlite3-$num" ] && continue
  echo "building SQLite $num"
  curl -fsSL -o "$work/$num.zip" "https://www.sqlite.org/$year/sqlite-amalgamation-$num.zip"
  mkdir "$work/$num" && unzip -q -j "$work/$num.zip" -d "$work/$num"
  cc -O2 -w -DSQLITE_THREADSAFE=0 -o "$cache/sqlite3-$num" "$work/$num/shell.c" "$work/$num/sqlite3.c" -lm
done

cat > "$work/indexes.sql" <<'SQL'
CREATE INDEX people_name_dob ON people (name, dob DESC);
CREATE UNIQUE INDEX people_email ON people (email);
CREATE INDEX people_live ON people (city) WHERE deleted_at IS NULL AND age >= 18;
CREATE INDEX people_status ON people (status) WHERE status IN ('open', 'held') OR age BETWEEN 1 AND 9;
CREATE INDEX people_lower_name ON people (lower(name));
CREATE INDEX people_upper_trim ON people (upper(trim(city)));
CREATE INDEX people_nick ON people (nick);
CREATE INDEX people_score ON people (score);
SQL

queries=(
  "count(*) FROM people %s WHERE name = 'Name13'"
  "count(*) FROM people %s WHERE email = 'user42'"
  "count(*) FROM people %s WHERE nick = 'nick7'"
  "count(*) FROM people %s WHERE city = 'paris' AND deleted_at IS NULL AND age >= 18"
  "count(*) FROM people %s WHERE status = 'held'"
  "count(*) FROM people %s WHERE lower(name) = 'ångström 7'"
  "count(*) FROM people %s WHERE upper(trim(city)) = 'MÜNCHEN'"
  "count(*) FROM people %s WHERE score > 50.2"
)

failed=0
for v in $VERSIONS; do
  app=$cache/sqlite3-${v#*:}
  version=$("$app" :memory: 'SELECT sqlite_version()')
  for mode in delete wal; do
    db=$work/$version-$mode.db
    "$app" "$db" "PRAGMA journal_mode=$mode;
      CREATE TABLE people (id INTEGER PRIMARY KEY, name TEXT, email TEXT, nick TEXT COLLATE NOCASE, city TEXT,
        status TEXT, age INTEGER, score REAL, dob TEXT, deleted_at TEXT);
      WITH RECURSIVE n(i) AS (SELECT 1 UNION ALL SELECT i+1 FROM n WHERE i < 20000)
      INSERT INTO people (name, email, nick, city, status, age, score, dob, deleted_at)
      SELECT CASE i % 7 WHEN 0 THEN 'Ångström ' || i WHEN 1 THEN 'ÉCOLE' || i WHEN 2 THEN 'straße' || i
               WHEN 3 THEN 'a ' || char(10) || i WHEN 4 THEN 'İstanbul' || i WHEN 5 THEN 'abc' || char(9) || ' ' || i
               ELSE 'Name' || i END,
        'user' || i,
        CASE i % 3 WHEN 0 THEN 'NICK' || (i % 500) WHEN 1 THEN 'nick' || (i % 500) ELSE 'Ñick' || (i % 500) END,
        CASE i % 4 WHEN 0 THEN '  London ' WHEN 1 THEN 'paris' WHEN 2 THEN 'MÜNCHEN' ELSE char(9) || 'oslo' || char(10) END,
        CASE i % 4 WHEN 0 THEN 'open' WHEN 1 THEN 'held' WHEN 2 THEN 'void' ELSE NULL END,
        i % 90, (i % 1000) * 0.1 + 0.2, date('1950-01-01', '+' || (i % 20000) || ' days'),
        CASE WHEN i % 10 = 0 THEN '2024-01-01' END
      FROM n;" > /dev/null
    problems=()
    "$N" apply -allow-collations-and-functions -indexes "$work/indexes.sql" "$db" > /dev/null || problems+=("apply failed")
    [ "$("$app" "$db" 'PRAGMA integrity_check')" = ok ] || problems+=("integrity_check")
    for q in "${queries[@]}"; do
      a=$("$app" "$db" "SELECT $(printf "$q" "")") b=$("$app" "$db" "SELECT $(printf "$q" "NOT INDEXED")")
      [ "$a" = "$b" ] || problems+=("$q: $a indexed, $b not")
    done
    [ "$("$app" "$db" "INSERT INTO people (name, email, nick, city) VALUES ('New', 'new', 'NEW', 'paris'); PRAGMA integrity_check;")" = ok ] || problems+=("write")
    dup=$("$app" "$db" "INSERT INTO people (email) VALUES ('user42')" 2>&1 || true)
    [[ $dup == *UNIQUE* ]] || problems+=("UNIQUE not enforced")
    "$N" plan -exit-code -allow-collations-and-functions -indexes "$work/indexes.sql" "$db" > /dev/null || problems+=("not in sync")
    if [ ${#problems[@]} -eq 0 ]; then
      echo "SQLite $version ($mode): ok"
    else
      failed=1; echo "SQLite $version ($mode): ${problems[*]}"
    fi
  done
done
exit $failed
