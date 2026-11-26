#!/usr/bin/env python3
"""Delivery contract: the shipped UI must have a live process and a health path."""
from pathlib import Path
import unittest

ROOT = Path(__file__).resolve().parent.parent


class DeliveryWebTests(unittest.TestCase):
    def read(self, name):
        return (ROOT / name).read_text(encoding="utf-8")

    def test_docker_runs_and_probes_both_processes(self):
        dockerfile = self.read("Dockerfile")
        start = self.read("deploy/docker/start.sh")
        self.assertIn("COPY deploy/docker/start.sh /usr/local/bin/levee-start", dockerfile)
        self.assertIn('CMD ["/usr/local/bin/levee-start", "--addr", ":9090"]', dockerfile)
        self.assertIn("EXPOSE 8080 8081 9090", dockerfile)
        self.assertIn("ENV LEVEE_SERVER_DATA_DIR=/var/lib/levee/data", dockerfile)
        self.assertIn("http://127.0.0.1:8081/", dockerfile)
        self.assertIn("http://127.0.0.1:8080/healthz", dockerfile)
        self.assertIn("levee serve \"$@\" &", start)
        self.assertIn("levee web --addr \"$web_addr\" --api \"$web_api\" &", start)
        self.assertIn("trap shutdown TERM INT", start)
        self.assertIn("mkdir -p \"$data_dir\" \"$template_dir\"", start)
        self.assertIn("kill -0 \"$serve_pid\"", start)
        self.assertIn("kill -0 \"$web_pid\"", start)

    def test_helm_has_web_sidecar_service_port_and_probe(self):
        deployment = self.read("deploy/helm/levee/templates/deployment.yaml")
        service = self.read("deploy/helm/levee/templates/service.yaml")
        values = self.read("deploy/helm/levee/values.yaml")
        self.assertIn("webPort: 8081", values)
        self.assertIn("- name: web", deployment)
        self.assertIn("http://127.0.0.1:{{ .Values.service.httpPort }}", deployment)
        self.assertIn("path: /", deployment)
        self.assertIn("containerPort: {{ .Values.service.webPort }}", deployment)
        self.assertIn("port: {{ .Values.service.webPort }}", service)
        self.assertIn("targetPort: web", service)

    def test_systemd_installs_and_starts_the_web_unit(self):
        unit = self.read("deploy/systemd/levee-web.service")
        install = self.read("deploy/baremetal/install.sh")
        self.assertIn("levee web --addr :8081 --api http://127.0.0.1:8080", unit)
        self.assertIn("deploy/systemd/levee-web.service", install)
        self.assertIn("$UNIT_DIR/levee-web.service", install)
        self.assertIn("systemctl enable --now levee levee-web", install)


if __name__ == "__main__":
    unittest.main()
