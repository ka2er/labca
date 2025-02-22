#!/usr/bin/env bash

set -e

flag_skip_redis=true
cloneDir=$(dirname $0)

# For legacy mode, when called from the install script...
SUDO="$1"
boulderLabCADir="${2:-labca}"

[ -d "$boulderLabCADir/config" ] || mkdir -p "$boulderLabCADir/config"


$SUDO patch -p1 -o "$boulderLabCADir/entrypoint.sh" < $cloneDir/patches/entrypoint.patch
cp test/startservers.py "$boulderLabCADir/startservers.py"

$SUDO patch -p1 -o "$boulderLabCADir/config/ca-a.json" < $cloneDir/patches/test_config_ca_a.patch
$SUDO patch -p1 -o "$boulderLabCADir/config/ca-b.json" < $cloneDir/patches/test_config_ca_b.patch

$SUDO patch -p1 -o "$boulderLabCADir/config/expiration-mailer.json" < $cloneDir/patches/config_expiration-mailer.patch
$SUDO patch -p1 -o "$boulderLabCADir/config/notify-mailer.json" < $cloneDir/patches/config_notify-mailer.patch
$SUDO patch -p1 -o "$boulderLabCADir/config/bad-key-revoker.json" < $cloneDir/patches/config_bad-key-revoker.patch
$SUDO patch -p1 -o "$boulderLabCADir/config/ocsp-responder.json" < $cloneDir/patches/config_ocsp-responder.patch
$SUDO patch -p1 -o "$boulderLabCADir/config/publisher.json" < $cloneDir/patches/config_publisher.patch
$SUDO patch -p1 -o "$boulderLabCADir/config/wfe2.json" < $cloneDir/patches/config_wfe2.patch
$SUDO patch -p1 -o "$boulderLabCADir/config/crl-storer.json" < $cloneDir/patches/config_crl-storer.patch
$SUDO patch -p1 -o "$boulderLabCADir/config/crl-updater.json" < $cloneDir/patches/config_crl-updater.patch
$SUDO patch -p1 -o "$boulderLabCADir/config/ra.json" < $cloneDir/patches/config_ra.patch
$SUDO patch -p1 -o "$boulderLabCADir/config/akamai-purger.json" < $cloneDir/patches/config_akamai-purger.patch

cp test/config/va*.json "$boulderLabCADir/config/"
perl -i -p0e "s/\"dnsProvider\": {.*?\t\t},/\"dnsResolvers\": [\n\t\t\t\"127.0.0.1:8053\",\n\t\t\t\"127.0.0.1:8054\"\n\t\t],/igs" $boulderLabCADir/config/va.json
perl -i -p0e "s/\"dnsProvider\": {.*?\t\t},/\"dnsResolvers\": [\n\t\t\t\"127.0.0.1:8053\",\n\t\t\t\"127.0.0.1:8054\"\n\t\t],/igs" $boulderLabCADir/config/va-remote-a.json
perl -i -p0e "s/\"dnsProvider\": {.*?\t\t},/\"dnsResolvers\": [\n\t\t\t\"127.0.0.1:8053\",\n\t\t\t\"127.0.0.1:8054\"\n\t\t],/igs" $boulderLabCADir/config/va-remote-b.json

if [ "$flag_skip_redis" == true ]; then
    perl -i -p0e "s/\n    \"redis\": \{\n.*?    \},//igs" $boulderLabCADir/config/ocsp-responder.json
fi

for f in $(grep -l boulder-proxysql $boulderLabCADir/secrets/*); do sed -i -e "s/proxysql:6033/mysql:3306/" $f; done

cd "$boulderLabCADir"
sed -i -e "s/test-ca2.pem/test-ca.pem/" config/ocsp-responder.json
sed -i -e "s/test-ca2.pem/test-ca.pem/" config/publisher.json
sed -i -e "s/test-ca2.pem/test-ca.pem/" config/ra.json
sed -i -e "s/test-ca2.pem/test-ca.pem/" config/wfe2.json
sed -i -e "s|/hierarchy/intermediate-cert-rsa-a.pem|labca/test-ca.pem|" config/akamai-purger.json
sed -i -e "s|/hierarchy/intermediate-cert-rsa-a.pem|labca/test-ca.pem|" config/ocsp-responder.json
sed -i -e "s|/hierarchy/intermediate-cert-rsa-a.pem|labca/test-ca.pem|" config/publisher.json
sed -i -e "s|/hierarchy/intermediate-cert-rsa-a.pem|labca/test-ca.pem|" config/ra.json
sed -i -e "s|/hierarchy/intermediate-cert-rsa-a.pem|labca/test-ca.pem|" config/wfe2.json
sed -i -e "s|/hierarchy/intermediate-cert-rsa-a.pem|labca/test-ca.pem|" config/crl-storer.json
sed -i -e "s|/hierarchy/intermediate-cert-rsa-a.pem|labca/test-ca.pem|" config/crl-updater.json
sed -i -e "s|/hierarchy/intermediate-cert-rsa-a.pem|labca/test-ca.pem|" config/ra.json
sed -i -e "s|/hierarchy/intermediate-cert-rsa-a.pem|labca/test-ca.pem|" v2_integration.py
sed -i -e "s|/hierarchy/root-cert-rsa.pem|labca/test-root.pem|" cert-ceremonies/root-ceremony-rsa.yaml
sed -i -e "s|/hierarchy/root-cert-rsa.pem|labca/test-root.pem|" cert-ceremonies/root-crl-rsa.yaml
sed -i -e "s|/hierarchy/root-cert-rsa.pem|labca/test-root.pem|" cert-ceremonies/intermediate-ceremony-rsa.yaml
sed -i -e "s|/hierarchy/root-cert-rsa.pem|labca/test-root.pem|" config/publisher.json
sed -i -e "s|/hierarchy/root-cert-rsa.pem|labca/test-root.pem|" config/wfe2.json
sed -i -e "s|/hierarchy/root-cert-rsa.pem|labca/test-root.pem|" integration-test.py
sed -i -e "s|/hierarchy/root-cert-rsa.pem|labca/test-root.pem|" helpers.py
sed -i -e "s/5001/443/g" config/va.json
sed -i -e "s/5002/80/g" config/va.json
sed -i -e "s/5001/443/g" config/va-remote-a.json
sed -i -e "s/5002/80/g" config/va-remote-a.json
sed -i -e "s/5001/443/g" config/va-remote-b.json
sed -i -e "s/5002/80/g" config/va-remote-b.json
sed -i -e "s|letsencrypt/boulder|hakwerk/labca|" config/wfe2.json
sed -i -e "s|1.2.3.4|1.3.6.1.4.1.44947.1.1.1|g" config/ca-a.json
sed -i -e "s|1.2.3.4|1.3.6.1.4.1.44947.1.1.1|g" config/ca-b.json
sed -i -e "s/ocspURL.Path = encodedReq/ocspURL.Path += encodedReq/" ocsp/helper/helper.go
sed -i -e "s/\"dnsTimeout\": \".*\"/\"dnsTimeout\": \"3s\"/" config/ra.json
sed -i -e "s/\"dnsTimeout\": \".*\"/\"dnsTimeout\": \"3s\"/" config/va.json
sed -i -e "s/\"dnsTimeout\": \".*\"/\"dnsTimeout\": \"3s\"/" config/va-remote-a.json
sed -i -e "s/\"dnsTimeout\": \".*\"/\"dnsTimeout\": \"3s\"/" config/va-remote-b.json
sed -i -e "s/\"stdoutlevel\": 4,/\"stdoutlevel\": 6,/" config/ca-a.json
sed -i -e "s/\"stdoutlevel\": 4,/\"stdoutlevel\": 6,/" config/ca-b.json
sed -i -e "s/\"stdoutlevel\": 4,/\"stdoutlevel\": 6,/" config/va-remote-a.json
sed -i -e "s/\"stdoutlevel\": 4,/\"stdoutlevel\": 6,/" config/va-remote-b.json

if [ "$flag_skip_redis" == true ]; then
    sed -i -e "s/^\(.*wait-for-it.sh.*4218\)/#\1/" entrypoint.sh
fi

for file in `find . -type f | grep -v .git`; do
    sed -i -e "s|test/|labca/|g" $file
done

sed -i -e "s/names/name\(s\)/" config/expiration-mailer.gotmpl

rm test-ca2.pem
