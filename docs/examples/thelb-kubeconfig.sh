#!/bin/sh
# theLB 0.1.0-beta.1, as `thelb generate k8s kubeconfig` printed it. Run it where kubectl
# reaches the cluster, after applying what `thelb generate k8s` prints: it prints
# the kubeconfig of directors outside the cluster, or under systemd on its
# nodes - with the token of the ServiceAccount thelb-director.
#   sh thelb-kubeconfig.sh > thelb.kubeconfig
# Then, on each director:
#   sudo install -D -m 600 thelb.kubeconfig /etc/thelb/kubeconfig
# and start it with --kubeconfig /etc/thelb/kubeconfig.
set -eu
server=$(kubectl config view --minify -o jsonpath='{.clusters[0].cluster.server}')
ca=$(kubectl -n thelb get secret thelb-director-token -o jsonpath='{.data.ca\.crt}')
token=$(kubectl -n thelb get secret thelb-director-token -o jsonpath='{.data.token}' | base64 -d)
if [ -z "$token" ]; then
	echo "the Secret thelb/thelb-director-token has no token yet: apply what thelb generate k8s prints, then try again" >&2
	exit 1
fi
cat <<EOF
apiVersion: v1
kind: Config
clusters:
  - name: cluster
    cluster:
      server: $server
      certificate-authority-data: $ca
users:
  - name: thelb-director
    user:
      token: $token
contexts:
  - name: thelb
    context:
      cluster: cluster
      user: thelb-director
current-context: thelb
EOF
