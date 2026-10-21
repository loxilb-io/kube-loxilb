#!/bin/bash
# One HTTP request on stdin, the file at its path on stdout - run by socat for
# each connection to the stand-in OIDC provider. GET of files under /www only.
read -r method path _
while read -r line; do [[ -z "${line%$'\r'}" ]] && break; done
path="${path%%\?*}"
file="/www$path"
if [[ "$method" == GET && "$path" != *..* && -f "$file" ]]; then
  printf 'HTTP/1.1 200 OK\r\nContent-Type: application/json\r\nContent-Length: %d\r\nConnection: close\r\n\r\n' \
    "$(wc -c < "$file")"
  cat "$file"
else
  printf 'HTTP/1.1 404 Not Found\r\nContent-Length: 0\r\nConnection: close\r\n\r\n'
fi
