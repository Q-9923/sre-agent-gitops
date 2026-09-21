#!/usr/bin/env bash

set -euo pipefail

SCRIPT_DIR=$(
  cd -- "$(dirname -- "${BASH_SOURCE[0]}")" &&
    pwd
)

APP_DIR=$(
  cd -- "${SCRIPT_DIR}/.." &&
    pwd
)

MANIFEST="${APP_DIR}/networkpolicy.yaml"
TEMP_ROOT="${APP_DIR}/.tmp"

mkdir -p "${TEMP_ROOT}"

if [[ ! -f "${MANIFEST}" ]]; then
  echo "ERROR: missing networkpolicy.yaml"
  exit 1
fi

TEMP_DIR=$(
  mktemp -d "${TEMP_ROOT}/network-policy.XXXXXX"
)

JSON_FILE="${TEMP_DIR}/networkpolicy.json"

cleanup() {
  rm -f -- "${JSON_FILE}"
  rmdir -- "${TEMP_DIR}" 2>/dev/null || true
}

trap cleanup EXIT

kubectl apply \
  --dry-run=client \
  -f "${MANIFEST}" \
  -o json >"${JSON_FILE}"

jq -e '
  def egress_rule($to; $protocol; $port):
    any(
      .spec.egress[]?;
      .to == $to
       and any(.ports[]?; .protocol == $protocol and .port == $port)
    );

  .apiVersion == "networking.k8s.io/v1"
  and .kind == "NetworkPolicy"
  and .metadata.name == "sre-agent-v2-network-policy"
  and .metadata.namespace == "sre-agent-system"
  and .spec.podSelector.matchLabels == {
    "app.kubernetes.io/name": "sre-agent-v2",
    "app.kubernetes.io/component": "agent"
  }
  and (.spec.policyTypes | sort) == ["Egress", "Ingress"]
  and (
    any(
      .spec.ingress[]?;
      .from == [{
        "namespaceSelector": {
          "matchLabels": {
            "kubernetes.io/metadata.name": "monitoring"
          }
        }
      }]
      and .ports == [{"protocol": "TCP", "port": 8080}]
    )
  )
  and egress_rule(
    [{
      "namespaceSelector": {
        "matchLabels": {
          "kubernetes.io/metadata.name": "kube-system"
        }
      },
      "podSelector": {
        "matchLabels": {
          "k8s-app": "kube-dns"
        }
      }
    }];
    "UDP";
    53
  )
  and egress_rule(
    [{
      "namespaceSelector": {
        "matchLabels": {
          "kubernetes.io/metadata.name": "kube-system"
        }
      },
      "podSelector": {
        "matchLabels": {
          "k8s-app": "kube-dns"
        }
      }
    }];
    "TCP";
    53
  )
  and egress_rule(
    [{
      "namespaceSelector": {
        "matchLabels": {
          "kubernetes.io/metadata.name": "monitoring"
        }
      },
      "podSelector": {
        "matchLabels": {
          "app.kubernetes.io/name": "prometheus",
          "operator.prometheus.io/name": "prometheus-stack-kube-prom-prometheus"
        }
      }
    }];
    "TCP";
    9090
  )
  and egress_rule(
    [{
      "namespaceSelector": {
        "matchLabels": {
          "kubernetes.io/metadata.name": "ai-services"
        }
      },
      "podSelector": {
        "matchLabels": {
          "app": "ollama"
        }
      }
    }];
    "TCP";
    11434
  )
  and egress_rule(
    [{
      "ipBlock": {
        "cidr": "10.96.0.1/32"
      }
    }];
    "TCP";
    443
  )
  and egress_rule(
    [{
      "ipBlock": {
        "cidr": "192.168.30.11/32"
      }
    }];
    "TCP";
    6443
  )
  and all(
    .spec.egress[]?;
    (.to | length) > 0
  )
  and all(
    .spec.egress[]?.to[]?;
    (.ipBlock.cidr // "") != "0.0.0.0/0"
  )
' "${JSON_FILE}" >/dev/null

echo "NETWORK_POLICY_CONTRACT_VERIFIED"
