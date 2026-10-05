import importlib.util
import os
from pathlib import Path
import subprocess
import tempfile
import unittest
from unittest.mock import patch


spec = importlib.util.spec_from_file_location("fork_release", Path(__file__).with_name("fork_release.py"))
release = importlib.util.module_from_spec(spec)
spec.loader.exec_module(release)


class ForkReleaseTest(unittest.TestCase):
    def test_main_push_prepares_and_pushes_a_completed_release(self):
        with tempfile.TemporaryDirectory() as directory:
            previous = os.getcwd()
            work = Path(directory) / "work"
            work.mkdir()
            os.chdir(work)
            try:
                release.git("init", "-b", "main")
                release.git("config", "user.name", "Test")
                release.git("config", "user.email", "test@example.invalid")
                Path(".github").mkdir()
                Path(".github/fork-release-baseline.txt").write_text("v1.0.0-rc.2\nv1.0.0-rc.10\n")
                Path("common.txt").write_text("base\n")
                release.git("add", ".")
                release.git("commit", "-m", "base")
                base = release.git("rev-parse", "HEAD")
                for tag in ["v1.0.0-rc.2", "v1.0.0-rc.10", "v1.0.0"]:
                    release.git("update-ref", f"refs/upstream-tags/{tag}", base)
                Path("release-only.txt").write_text("resolved release\n")
                release.git("add", ".")
                release.git("commit", "-m", "release resolution")
                resolved = release.git("rev-parse", "HEAD")
                release.git("checkout", "-B", "main", base)
                Path("main-update.txt").write_text("merged PR\n")
                release.git("add", ".")
                release.git("commit", "-m", "main update")
                main_sha = release.git("rev-parse", "HEAD")
                release.git("init", "--bare", "../origin.git")
                release.git("remote", "add", "origin", "../origin.git")
                release.git("push", "origin", "main", f"{base}:refs/heads/fork-release/v1.0.0-rc.2",
                            f"{resolved}:refs/heads/fork-release/v1.0.0-rc.10")
                release.git("fetch", "origin")
                release.git("tag", "fork-built/v1.0.0-rc.10", resolved)
                output = Path(directory) / "output"
                summary = Path(directory) / "summary"
                with patch.dict(os.environ, {
                    "REBUILD_LATEST": "true", "REQUESTED_TAG": "",
                    "GITHUB_REPOSITORY": "AlexeyVatolin/new-api",
                    "GITHUB_OUTPUT": str(output), "GITHUB_STEP_SUMMARY": str(summary),
                }):
                    release.main()
                values = dict(line.split("=", 1) for line in output.read_text().splitlines())
                self.assertEqual(values["tag"], "v1.0.0-rc.10")
                self.assertEqual(values["main_sha"], main_sha)
                self.assertEqual(values["latest"], "true")
                self.assertEqual(values["image"], "ghcr.io/alexeyvatolin/new-api")
                self.assertEqual(values["version"], f"v1.0.0-rc.10-fork.{values['sha'][:12]}")
                for parent in [main_sha, resolved]:
                    release.git("merge-base", "--is-ancestor", parent, values["sha"])
                self.assertEqual(release.git("rev-parse", "main"), main_sha)
                self.assertEqual(release.git("ls-remote", "origin", "refs/heads/fork-release/v1.0.0-rc.10").split()[0], values["sha"])
                self.assertIn("Trigger: main update", summary.read_text())
            finally:
                os.chdir(previous)

    def test_main_update_rebuilds_the_latest_existing_release(self):
        tags = ["v1.0.0-alpha.1", "v1.0.0-rc.2", "v1.0.0-rc.10", "v1.0.0"]
        branches = {tags[0], tags[2], "v99.0.0"}
        self.assertEqual(release.select_tag(
            tags, set(tags), set(tags), release_branches=branches, rebuild_latest=True,
        ), tags[2])
        self.assertEqual(release.select_tag(
            tags, set(tags), set(tags), release_branches=set(tags), rebuild_latest=True,
        ), tags[-1])
        self.assertEqual(release.select_tag(tags, set(tags), set(tags), rebuild_latest=True), tags[-1])
        self.assertEqual(release.select_tag([], set(), set(), rebuild_latest=True), "")
        # An explicit tag still overrides automatic selection; scheduled runs retain their queue.
        self.assertEqual(release.select_tag(
            tags, set(tags), set(tags), tags[0], release_branches=branches, rebuild_latest=True,
        ), tags[0])
        self.assertEqual(release.select_tag(tags, {tags[0]}, {tags[1]}, release_branches=branches), tags[2])

    def test_new_tags_are_queued_and_failed_builds_are_retried(self):
        tags = ["v1.0.0-rc.40", "v1.0.0-rc.41", "v1.0.0-rc.42"]
        baseline = {tags[0]}
        self.assertEqual(release.select_tag(tags, baseline, set()), tags[1])
        self.assertEqual(release.select_tag(tags, baseline, {tags[1]}), tags[2])
        self.assertEqual(release.select_tag(tags, baseline, set(tags)), "")
        self.assertEqual(release.select_tag(tags, baseline, set(tags), tags[0]), tags[0])
        for invalid in ["main", "nightly-20261005", "v1.0.0-rc.99", "v1.0.0\ninjected=true"]:
            with self.subTest(tag=invalid), self.assertRaises(ValueError):
                release.select_tag(tags, baseline, set(), invalid)

    def test_merge_preserves_both_histories_and_leaves_main_intact(self):
        with tempfile.TemporaryDirectory() as directory:
            previous = os.getcwd()
            os.chdir(directory)
            try:
                release.git("init", "-b", "main")
                release.git("config", "user.name", "Test")
                release.git("config", "user.email", "test@example.invalid")
                Path("common.txt").write_text("base\n")
                release.git("add", ".")
                release.git("commit", "-m", "base")
                base = release.git("rev-parse", "HEAD")
                Path("custom.txt").write_text("fork patch\n")
                release.git("add", ".")
                release.git("commit", "-m", "fork patch")
                main_sha = release.git("rev-parse", "HEAD")
                release.git("checkout", "--detach", base)
                Path("upstream.txt").write_text("new release\n")
                release.git("add", ".")
                release.git("commit", "-m", "upstream release")
                upstream_sha = release.git("rev-parse", "HEAD")
                release.git("update-ref", "refs/upstream-tags/v1.0.0", upstream_sha)
                branch, merged = release.merge_release("v1.0.0", main_sha)
                self.assertEqual(branch, "fork-release/v1.0.0")
                self.assertEqual(release.git("rev-parse", "main"), main_sha)
                self.assertEqual(Path("custom.txt").read_text(), "fork patch\n")
                self.assertEqual(Path("upstream.txt").read_text(), "new release\n")
                for parent in [main_sha, upstream_sha]:
                    release.git("merge-base", "--is-ancestor", parent, merged)

                # Retry uses the already resolved/pushed release branch.
                release.git("update-ref", f"refs/remotes/origin/{branch}", merged)
                self.assertEqual(release.merge_release("v1.0.0", main_sha)[1], merged)

                # A main update builds on the release branch instead of recreating it from the tag.
                Path("release-only.txt").write_text("preserved release resolution\n")
                release.git("add", ".")
                release.git("commit", "-m", "release resolution")
                release_sha = release.git("rev-parse", "HEAD")
                release.git("update-ref", f"refs/remotes/origin/{branch}", release_sha)
                release.git("checkout", "main")
                Path("main-update.txt").write_text("new fork change\n")
                release.git("add", ".")
                release.git("commit", "-m", "main update")
                main_sha = release.git("rev-parse", "HEAD")
                _, refreshed = release.merge_release("v1.0.0", main_sha)
                for parent in [release_sha, main_sha]:
                    release.git("merge-base", "--is-ancestor", parent, refreshed)
                self.assertEqual(Path("release-only.txt").read_text(), "preserved release resolution\n")
                self.assertEqual(Path("main-update.txt").read_text(), "new fork change\n")
                self.assertEqual(release.git("rev-parse", "main"), main_sha)

                # A conflicting release fails without advancing main or retaining merge state.
                release.git("checkout", "main")
                Path("common.txt").write_text("fork\n")
                release.git("commit", "-am", "fork edit")
                conflicting_main = release.git("rev-parse", "HEAD")
                release.git("checkout", "--detach", base)
                Path("common.txt").write_text("upstream\n")
                release.git("commit", "-am", "upstream edit")
                release.git("update-ref", "refs/upstream-tags/v1.0.1", release.git("rev-parse", "HEAD"))
                with self.assertRaisesRegex(RuntimeError, "common.txt"):
                    release.merge_release("v1.0.1", conflicting_main)
                self.assertEqual(release.git("rev-parse", "main"), conflicting_main)
                self.assertFalse(Path(".git/MERGE_HEAD").exists())
            finally:
                os.chdir(previous)


if __name__ == "__main__":
    unittest.main()
