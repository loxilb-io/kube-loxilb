#!/usr/bin/env python3
"""Stand-in OIDC provider for the credential checks.

  oidc-stub.py init <dir> <issuer>
      a signing key, and the discovery document and JWKS under <dir>/www at
      the paths a Keycloak realm serves them
  oidc-stub.py mint <dir> <issuer> [--aud A] [--tenant T] [--roles r,r]
                    [--ttl S] [--foreign]
      a signed token; --foreign signs it with a key the JWKS does not hold,
      under the same kid

The documents are static, so the container serving them needs no Python:
oidc-serve.sh answers each request with the file at its path. Tokens are
minted here, on the host, with the key init wrote.
"""
import argparse, base64, json, os, sys, time
from urllib.parse import urlparse

import jwt
from cryptography.hazmat.primitives import serialization
from cryptography.hazmat.primitives.asymmetric import rsa

KID = "e2e-1"


def b64url_uint(n):
    raw = n.to_bytes((n.bit_length() + 7) // 8, "big")
    return base64.urlsafe_b64encode(raw).rstrip(b"=").decode()


def new_key():
    return rsa.generate_private_key(public_exponent=65537, key_size=2048)


def write(path, doc):
    os.makedirs(os.path.dirname(path), exist_ok=True)
    with open(path, "w") as f:
        json.dump(doc, f)


def init(d, issuer):
    key = new_key()
    with open(os.path.join(d, "key.pem"), "wb") as f:
        f.write(key.private_bytes(serialization.Encoding.PEM,
                                  serialization.PrivateFormat.PKCS8,
                                  serialization.NoEncryption()))
    pub = key.public_key().public_numbers()
    jwks_uri = issuer.rstrip("/") + "/protocol/openid-connect/certs"
    base = os.path.join(d, "www") + urlparse(issuer).path.rstrip("/")
    write(base + "/.well-known/openid-configuration",
          {"issuer": issuer, "jwks_uri": jwks_uri})
    write(os.path.join(d, "www") + urlparse(jwks_uri).path,
          {"keys": [{"kty": "RSA", "kid": KID, "use": "sig", "alg": "RS256",
                     "n": b64url_uint(pub.n), "e": b64url_uint(pub.e)}]})


def mint(d, issuer, aud, tenant, roles, ttl, foreign):
    if foreign:
        key = new_key()
    else:
        with open(os.path.join(d, "key.pem"), "rb") as f:
            key = serialization.load_pem_private_key(f.read(), password=None)
    now = int(time.time())
    claims = {"iss": issuer, "aud": aud, "sub": "e2e-user", "iat": now, "exp": now + ttl,
              "preferred_username": "e2e", "realm_access": {"roles": roles}}
    if tenant:
        claims["tenant_id"] = tenant
    return jwt.encode(claims, key, algorithm="RS256", headers={"kid": KID})


def main():
    p = argparse.ArgumentParser()
    p.add_argument("cmd", choices=["init", "mint"])
    p.add_argument("dir")
    p.add_argument("issuer")
    p.add_argument("--aud", default="kube-e2e")
    p.add_argument("--tenant", default="t1")
    p.add_argument("--roles", default="")
    p.add_argument("--ttl", type=int, default=3600)
    p.add_argument("--foreign", action="store_true")
    a = p.parse_args()

    os.makedirs(a.dir, exist_ok=True)
    if a.cmd == "init":
        init(a.dir, a.issuer)
    else:
        roles = [r for r in a.roles.split(",") if r]
        print(mint(a.dir, a.issuer, a.aud, a.tenant, roles, a.ttl, a.foreign))


if __name__ == "__main__":
    sys.exit(main())
