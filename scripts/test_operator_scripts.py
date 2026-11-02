#!/usr/bin/env python3
"""Tests for the operator template's scripts and workflows.

Run with `python3 -I scripts/test_operator_scripts.py`. None of it needs Go,
Docker or a cluster: the scripts under test are plain shell that is exercised
with fake inputs, and the workflows are checked as text (the standard library
has no YAML parser, and the properties checked here are textual: which
triggers, which permissions, how actions are pinned).
"""

import os
import re
import shutil
import subprocess
import tempfile
import unittest
from pathlib import Path

ROOT = Path(__file__).resolve().parent.parent
WORKFLOWS = ROOT / ".github" / "workflows"
DIGEST = "sha256:" + "ab" * 32


def run(cmd, env=None, cwd=ROOT, **kw):
    full = {k: v for k, v in os.environ.items() if k not in ("OCI_REGISTRY", "OCI_IMAGE_NAME")}
    full.update(env or {})
    return subprocess.run(cmd, cwd=cwd, env=full, capture_output=True, text=True, check=False, **kw)


class CheckDispatch(unittest.TestCase):
    def check(self, tag="v1.2.3", ref="refs/heads/main", branch="main"):
        env = {"RELEASE_TAG": tag, "GITHUB_REF": ref, "DEFAULT_BRANCH": branch}
        return run(["scripts/extras/check-dispatch.sh"], env=env)

    def test_release_tag_passes(self):
        r = self.check()
        self.assertEqual(r.returncode, 0, r.stderr)
        self.assertEqual(r.stdout.strip(), "version=1.2.3")

    def test_prerelease_tag_passes(self):
        r = self.check(tag="v1.2.3-rc.1")
        self.assertEqual(r.stdout.strip(), "version=1.2.3-rc.1")

    def test_other_branch_is_refused(self):
        r = self.check(ref="refs/heads/feature")
        self.assertNotEqual(r.returncode, 0)
        self.assertIn("default branch", r.stderr)

    def test_a_tag_ref_is_refused(self):
        self.assertNotEqual(self.check(ref="refs/tags/v1.2.3").returncode, 0)

    def test_bad_tags_are_refused(self):
        for tag in ("1.2.3", "v1.2", "v01.2.3", "v1.2.3;rm", "v1.2.3 ", "main", "v1.2.3-", "$(id)"):
            with self.subTest(tag=tag):
                self.assertNotEqual(self.check(tag=tag).returncode, 0)

    def test_missing_variables_fail(self):
        self.assertNotEqual(run(["scripts/extras/check-dispatch.sh"], env={"RELEASE_TAG": ""}).returncode, 0)


class CheckAbsent(unittest.TestCase):
    """check-absent.sh against a fake curl that answers like ghcr.io."""

    def check(self, answers, *args, resume=False, refs=None):
        """answers maps a package path fragment to (http code, digest or None)."""
        with tempfile.TemporaryDirectory() as tmp:
            fake = Path(tmp) / "curl"
            lines = ["#!/usr/bin/env bash", 'case "$*" in', '  *"/token?"*) echo \'{"token":"t"}\' ;;']
            for fragment, (code, digest) in answers.items():
                header = f"Docker-Content-Digest: {digest}\r\n" if digest else ""
                lines.append(f'  *"/v2/{fragment}/manifests/"*) printf "HTTP/2 {code}\\r\\n{header}\\nHTTP_CODE={code}\\n" ;;')
            lines += ['  *) printf "\\nHTTP_CODE=500\\n" ;;', "esac", ""]
            fake.write_text("\n".join(lines))
            fake.chmod(0o755)
            env = {"PATH": f"{tmp}:{os.environ['PATH']}", "GITHUB_TOKEN": "x"}
            cmd = ["scripts/extras/check-absent.sh"] + (["--resume"] if resume else []) + ["1.0.0"]
            return run(cmd + (refs or ["chart=ghcr.io/o/charts/c", "bundle=ghcr.io/o/b"]), env=env)

    def test_absent_tags_pass(self):
        r = self.check({"o/charts/c": (404, None), "o/b": (404, None)})
        self.assertEqual((r.returncode, r.stdout), (0, ""))

    def test_an_existing_tag_is_refused(self):
        r = self.check({"o/charts/c": (200, DIGEST), "o/b": (404, None)})
        self.assertEqual(r.returncode, 1)
        self.assertIn("already exists", r.stderr)
        self.assertIn("resume=true", r.stderr)
        self.assertEqual(r.stdout, "")

    def test_resume_prints_the_digest_of_what_exists_and_nothing_for_the_rest(self):
        r = self.check({"o/charts/c": (200, DIGEST), "o/b": (404, None)}, resume=True)
        self.assertEqual(r.returncode, 0, r.stderr)
        self.assertEqual(r.stdout.strip(), f"chart_digest={DIGEST}")

    def test_resume_without_anything_published_publishes_everything(self):
        r = self.check({"o/charts/c": (404, None), "o/b": (404, None)}, resume=True)
        self.assertEqual((r.returncode, r.stdout), (0, ""))

    def test_resume_refuses_an_existing_tag_without_a_digest(self):
        r = self.check({"o/charts/c": (200, None), "o/b": (404, None)}, resume=True)
        self.assertEqual(r.returncode, 1)
        self.assertIn("no digest", r.stderr)

    def test_an_unclear_answer_is_refused_even_when_resuming(self):
        for resume in (False, True):
            with self.subTest(resume=resume):
                r = self.check({"o/charts/c": (500, None), "o/b": (404, None)}, resume=resume)
                self.assertEqual(r.returncode, 1)
                self.assertIn("HTTP 500", r.stderr)

    def test_other_registries_and_unlabelled_refs_are_refused(self):
        self.assertEqual(self.check({}, refs=["bundle=docker.io/o/b"]).returncode, 2)
        self.assertEqual(self.check({}, refs=["ghcr.io/o/b"]).returncode, 2)

    def test_usage_and_token(self):
        self.assertEqual(run(["scripts/extras/check-absent.sh", "1.0.0"]).returncode, 2)
        env = {k: v for k, v in os.environ.items() if k != "GITHUB_TOKEN"}
        r = subprocess.run(["scripts/extras/check-absent.sh", "1.0.0", "bundle=ghcr.io/o/b"], cwd=ROOT, env=env, capture_output=True, text=True, check=False)
        self.assertNotEqual(r.returncode, 0)

    def test_resume_skips_the_push_steps_of_what_exists(self):
        text = self.code_of("release-extras.yml")
        self.assertRegex(text, r"(?m)^      resume:\n(?:        .*\n)*?        default: false")
        for kind in ("chart", "bundle", "catalog"):
            with self.subTest(kind=kind):
                self.assertIn(f"if: steps.existing.outputs.{kind}_digest == ''", text)
                self.assertIn(f"steps.existing.outputs.{kind}_digest }}}}", text)

    def test_the_publish_job_runs_it_before_any_push(self):
        text = self.code_of("release-extras.yml")
        guard = text.index("check-absent.sh")
        self.assertLess(guard, text.index("helm push"))
        self.assertLess(guard, text.index("--push"))

    def test_a_resumed_artifact_is_signed_but_never_attested(self):
        extras = self.code_of("release-extras.yml")
        # Provenance is claimed only for what this run pushed ...
        for kind in ("CHART", "BUNDLE", "CATALOG"):
            with self.subTest(kind=kind):
                self.assertIn(f'"provenance": e["{kind}_BUILT"] != ""', extras)
                self.assertRegex(extras, rf"{kind}_BUILT: \$\{{\{{ steps\.[a-z]+\.outputs\.digest \}}\}}\n")
        # ... never for the released image, which another workflow built.
        self.assertIn('"provenance": False', extras)
        # An existing artifact without provenance is named in a warning.
        self.assertIn("gh attestation verify", extras)
        self.assertIn("::warning::", extras)
        self.assertIn("Cut a new patch version", extras)

    def test_signing_skips_a_digest_that_already_verifies(self):
        text = self.code_of("sign-attest.yml")
        sign = text[text.index("name: Sign\n") : text.index("Attest the build provenance")]
        self.assertLess(sign.index("cosign verify"), sign.index("cosign sign --yes"))
        self.assertIn("already signed", sign)

    def code_of(self, name):
        return "\n".join(line for line in (WORKFLOWS / name).read_text().splitlines() if not line.lstrip().startswith("#"))


class Refs(unittest.TestCase):
    def refs(self, **env):
        base = {"OCI_REGISTRY": "ghcr.io", "GITHUB_REPOSITORY": "Craftainer-Labs/Operator-DB"}
        base.update(env)
        return run(["scripts/extras/refs.sh"], env=base)

    def parse(self, r):
        self.assertEqual(r.returncode, 0, r.stderr)
        return dict(line.split("=", 1) for line in r.stdout.splitlines())

    def test_names_derive_from_the_repository(self):
        refs = self.parse(self.refs())
        self.assertEqual(refs["image"], "ghcr.io/craftainer-labs/operator-db")
        self.assertEqual(refs["bundle"], "ghcr.io/craftainer-labs/operator-db-bundle")
        self.assertEqual(refs["catalog"], "ghcr.io/craftainer-labs/operator-db-catalog")
        self.assertEqual(refs["chart_repo"], "oci://ghcr.io/craftainer-labs/charts")
        chart_name = re.search(r"^name:\s*(\S+)\s*$", (ROOT / "charts/chart/Chart.yaml").read_text(), re.M).group(1)
        self.assertEqual(refs["chart"], f"ghcr.io/craftainer-labs/charts/{chart_name}")

    def test_image_name_override_applies_like_the_image_release(self):
        refs = self.parse(self.refs(OCI_IMAGE_NAME="Acme/Custom"))
        self.assertEqual(refs["image"], "ghcr.io/acme/custom")
        self.assertEqual(refs["bundle"], "ghcr.io/acme/custom-bundle")

    def test_other_registries_are_refused(self):
        r = self.refs(OCI_REGISTRY="quay.io")
        self.assertNotEqual(r.returncode, 0)
        self.assertIn("ghcr.io only", r.stderr)

    def test_missing_registry_is_refused(self):
        r = run(["scripts/extras/refs.sh"], env={"GITHUB_REPOSITORY": "a/b"})
        self.assertNotEqual(r.returncode, 0)


class StageChart(unittest.TestCase):
    def test_the_digest_is_written_to_the_copy_only(self):
        original = (ROOT / "charts/chart/values.yaml").read_text()
        with tempfile.TemporaryDirectory() as tmp:
            r = run(["scripts/extras/stage-chart.sh", DIGEST, tmp])
            self.assertEqual(r.returncode, 0, r.stderr)
            staged = (Path(tmp) / "chart/values.yaml").read_text()
            self.assertIn(f'    digest: "{DIGEST}"', staged)
            self.assertNotIn('    digest: ""', staged)
            self.assertTrue((Path(tmp) / "chart/Chart.yaml").is_file())
        self.assertEqual((ROOT / "charts/chart/values.yaml").read_text(), original)
        self.assertIn('    digest: ""', original)

    def test_a_tag_is_not_a_digest(self):
        with tempfile.TemporaryDirectory() as tmp:
            for bad in ("latest", "sha256:abc", "sha256:" + "AB" * 32, DIGEST + "x", ""):
                with self.subTest(digest=bad):
                    self.assertNotEqual(run(["scripts/extras/stage-chart.sh", bad, tmp]).returncode, 0)

    def test_usage(self):
        self.assertEqual(run(["scripts/extras/stage-chart.sh"]).returncode, 2)


class BundleAndCatalog(unittest.TestCase):
    def test_bundle_requires_a_digest_reference(self):
        for image in ("ghcr.io/a/b:latest", "ghcr.io/a/b", "ghcr.io/a/b@sha256:abc", "controller:latest"):
            with self.subTest(image=image):
                r = run(["scripts/bundle.sh", image, "1.0.0", "alpha", "alpha"])
                self.assertEqual(r.returncode, 1)
                self.assertIn("digest reference", r.stderr)

    def test_bundle_requires_a_semantic_version(self):
        for version in ("v1.0.0", "1.0", "latest", "01.0.0"):
            with self.subTest(version=version):
                r = run(["scripts/bundle.sh", f"ghcr.io/a/b@{DIGEST}", version, "alpha", "alpha"])
                self.assertEqual(r.returncode, 1)
                self.assertIn("semantic version", r.stderr)

    def test_bundle_usage(self):
        self.assertEqual(run(["scripts/bundle.sh"]).returncode, 2)

    def test_catalog_usage(self):
        for args in ([], ["nope"], ["render"], ["validate", "x"], ["dockerfile"]):
            with self.subTest(args=args):
                self.assertEqual(run(["scripts/catalog.sh", *args]).returncode, 2)

    def test_catalog_dockerfile_pins_opm_by_digest(self):
        with tempfile.TemporaryDirectory() as tmp:
            out = Path(tmp) / "catalog.Dockerfile"
            self.assertEqual(run(["scripts/catalog.sh", "dockerfile", str(out)]).returncode, 0)
            text = out.read_text()
        self.assertRegex(text, r"(?m)^FROM quay\.io/operator-framework/opm:v[0-9.]+@sha256:[0-9a-f]{64}$")
        self.assertIn('"--cache-only"', text)
        self.assertIn("COPY catalog /configs", text)

    def test_render_without_a_bundle_fails_clearly(self):
        with tempfile.TemporaryDirectory() as tmp:
            shutil.copytree(ROOT / "scripts", Path(tmp) / "scripts")
            r = run(["scripts/catalog.sh", "render", "out"], cwd=tmp)
            self.assertNotEqual(r.returncode, 0)
            self.assertIn("make bundle", r.stderr)


class GoVersionCheck(unittest.TestCase):
    def tree(self, tmp):
        for rel in ("scripts/check-go-version.sh", "scripts/develop.d/10-go.sh", "Dockerfile", "go.mod"):
            dest = Path(tmp) / rel
            dest.parent.mkdir(parents=True, exist_ok=True)
            shutil.copy(ROOT / rel, dest)
        return Path(tmp)

    def test_the_repository_agrees(self):
        r = run(["scripts/check-go-version.sh"])
        self.assertEqual(r.returncode, 0, r.stderr)

    def test_a_drifted_dockerfile_fails(self):
        with tempfile.TemporaryDirectory() as tmp:
            t = self.tree(tmp)
            docker = (t / "Dockerfile").read_text()
            (t / "Dockerfile").write_text(re.sub(r"(?m)^ARG GO_VERSION=.*$", "ARG GO_VERSION=1.0.0", docker))
            r = run(["scripts/check-go-version.sh"], cwd=t)
            self.assertEqual(r.returncode, 1)
            self.assertIn("differ", r.stderr)

    def test_a_missing_version_fails(self):
        with tempfile.TemporaryDirectory() as tmp:
            t = self.tree(tmp)
            (t / "go.mod").write_text("module x\n\ngo 1.26.0\n")
            r = run(["scripts/check-go-version.sh"], cwd=t)
            self.assertEqual(r.returncode, 1)
            self.assertIn("go.mod", r.stderr)


class ToolInstaller(unittest.TestCase):
    def test_unknown_tool_is_refused_before_any_download(self):
        with tempfile.TemporaryDirectory() as tmp:
            r = run(["scripts/develop.d/20-k8s-tools.sh", "nope"], env={"INSTALL_DIR": tmp})
        self.assertEqual(r.returncode, 2)
        self.assertIn("unknown tool", r.stderr)

    def test_every_download_is_checksum_verified(self):
        text = (ROOT / "scripts/develop.d/20-k8s-tools.sh").read_text()
        installs = [i for i in re.findall(r"(?ms)^install_([a-z-]+)\(\) \{(.*?)^\}", text) if i[0] != "binary"]
        self.assertEqual(len(installs), 8)
        for name, body in installs:
            with self.subTest(tool=name):
                self.assertIn("verify ", body)

    def test_all_versions_are_pinned_with_renovate_annotations(self):
        text = (ROOT / "scripts/develop.d/20-k8s-tools.sh").read_text()
        pins = re.findall(r"(?m)^# renovate: .*\n[A-Z_]+_VERSION=\S+$", text)
        self.assertEqual(len(pins), 8)


@unittest.skipUnless(shutil.which("helm") and shutil.which("kustomize"), "helm and kustomize are not installed")
class ChartSync(unittest.TestCase):
    def test_the_chart_matches_config(self):
        r = run(["scripts/check-chart-sync.sh"])
        self.assertEqual(r.returncode, 0, r.stderr)

    def test_a_stale_chart_crd_is_caught(self):
        with tempfile.TemporaryDirectory() as tmp:
            t = Path(tmp)
            shutil.copytree(ROOT / "charts", t / "charts")
            shutil.copytree(ROOT / "config", t / "config")
            (t / "scripts").mkdir()
            shutil.copy(ROOT / "scripts/check-chart-sync.sh", t / "scripts")
            crd = next((t / "charts/chart/templates/crd").glob("*.yaml"))
            crd.write_text(re.sub(r"maxLength: (\d+)", lambda m: f"maxLength: {int(m.group(1)) + 1}", crd.read_text(), count=1))
            r = run(["scripts/check-chart-sync.sh"], cwd=t)
            self.assertEqual(r.returncode, 1)
            self.assertIn("differs from config/crd", r.stderr)


class Workflows(unittest.TestCase):
    """The shape of the release workflows, as text."""

    @staticmethod
    def read(name):
        return (WORKFLOWS / name).read_text()

    @staticmethod
    def code(text):
        """The file without comment-only lines."""
        return "\n".join(l for l in text.splitlines() if not l.lstrip().startswith("#"))

    def test_release_extras_is_dispatch_only(self):
        text = self.code(self.read("release-extras.yml"))
        on = re.search(r"(?ms)^on:\n(.*?)^\S", text + "\nX").group(1)
        self.assertIn("workflow_dispatch:", on)
        self.assertRegex(on, r"(?m)^\s+tag:")
        self.assertEqual(re.findall(r"(?m)^  ([a-z_]+):", on), ["workflow_dispatch"])
        for trigger in ("workflow_run", "pull_request_target", "push:", "schedule:", "release:"):
            self.assertNotIn(trigger, text)

    def test_release_extras_guards_the_dispatch(self):
        text = self.read("release-extras.yml")
        self.assertIn("scripts/extras/check-dispatch.sh", text)
        self.assertIn("gh release view", text)
        self.assertIn("compare/", text)
        self.assertRegex(text, r"(?m)^concurrency:\n  group: release-extras-\$\{\{ inputs\.tag \}\}\n  cancel-in-progress: false")
        for job in ("publish", "sign"):
            self.assertRegex(text, rf"(?m)^  {job}:")
            self.assertRegex(text, rf"(?ms)^  {job}:.*?needs: ")

    def test_only_the_reusable_workflow_signs(self):
        extras = self.code(self.read("release-extras.yml"))
        # The publishing job holds neither id-token nor attestations; the
        # signing job calls the reusable workflow, which does.
        publish = re.search(r"(?ms)^  publish:\n(.*?)^  sign:", extras).group(1)
        self.assertNotIn("id-token", publish)
        self.assertNotIn("attestations", publish)
        self.assertNotIn("cosign", publish)
        self.assertIn("uses: $/.github/workflows/sign-attest.yml", extras)

    def test_sign_attest_is_a_reusable_workflow(self):
        text = self.code(self.read("sign-attest.yml"))
        self.assertRegex(text, r"(?m)^on:\n  workflow_call:")
        self.assertEqual(len(re.findall(r"(?m)^on:\n  ([a-z_]+):", text)), 1)
        for needed in ("id-token: write", "attestations: write", "cosign sign --yes", "cosign verify", "uses: actions/attest@"):
            self.assertIn(needed, text)
        self.assertNotIn("secrets:", text)

    def test_every_action_is_pinned_by_sha(self):
        for path in sorted(WORKFLOWS.glob("*.yml")):
            if path.name not in ("release-extras.yml", "sign-attest.yml", "e2e.yml"):
                continue  # the base-owned workflows are not this template's
            for line in self.code(path.read_text()).splitlines():
                m = re.match(r"\s*(?:- )?uses:\s*(\S+)", line)
                if not m:
                    continue
                ref = m.group(1)
                with self.subTest(workflow=path.name, uses=ref):
                    if ref.startswith(("./", "$/")):
                        continue
                    self.assertRegex(ref, r"^[\w./-]+@[0-9a-f]{40}$")

    def test_no_workflow_uses_secrets_inherit_or_workflow_run(self):
        for name in ("release-extras.yml", "sign-attest.yml", "e2e.yml"):
            text = self.code(self.read(name))
            self.assertNotIn("secrets: inherit", text)
            self.assertNotIn("workflow_run", text)

    def test_expressions_do_not_reach_shell_text(self):
        # zizmor's template-injection audit, as a plain check on the new
        # workflows: no ${{ inputs.* }} / github.* inside a `run:` block.
        for name in ("release-extras.yml", "sign-attest.yml", "e2e.yml"):
            lines = self.read(name).splitlines()
            in_run = False
            indent = 0
            for i, line in enumerate(lines, 1):
                stripped = line.strip()
                if in_run and stripped and len(line) - len(line.lstrip()) <= indent:
                    in_run = False
                if in_run and "${{" in line:
                    self.fail(f"{name}:{i}: expression inside a run block: {stripped}")
                m = re.match(r"(\s*)(?:- )?run:\s*[|>]", line)
                if m:
                    in_run = True
                    indent = len(m.group(1)) + (2 if line.lstrip().startswith("- ") else 0)


class ImageMk(unittest.TestCase):
    def test_release_check_only_gains_prerequisites(self):
        text = (ROOT / "image.mk").read_text()
        lines = text.splitlines()
        for i, line in enumerate(lines):
            if re.match(r"release-check\s*:", line):
                nxt = lines[i + 1] if i + 1 < len(lines) else ""
                self.assertFalse(nxt.startswith("\t"), "release-check must not get a recipe")
        self.assertRegex(text, r"(?m)^release-check:.*helm-lint.*bundle-validate")

    def test_operator_targets_do_not_shadow_the_image_contract(self):
        contract = {
            "build", "sbom", "release-assets", "publish", "publish-packages", "verify-published",
            "release-check", "finalize", "image-smoke", "check-arch", "repo-check", "release-arches",
        }
        defined = set(re.findall(r"(?m)^([a-zA-Z][\w-]*):(?!=)", (ROOT / "operator.mk").read_text()))
        self.assertFalse(defined & contract, defined & contract)


if __name__ == "__main__":
    unittest.main()
