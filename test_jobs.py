import json
from contextlib import closing
from pathlib import Path
import tempfile
import unittest
from unittest.mock import patch
import jobs
from job_extractor.providers import collect_adzuna, missing_credentials
from job_extractor.scraping import collect_google_cse, collect_jsonld
from job_extractor.sources import collect


def example(ident="one"):
    return jobs.normalize("arbeitnow", {"slug": ident, "title": "Python Engineer", "company_name": "Example", "url": "https://example.com/jobs/" + ident, "description": "<p>Python &amp; SQL</p>", "remote": True})


class CollectorTests(unittest.TestCase):
    def test_keyword_boundaries(self):
        self.assertFalse(jobs.contains("Django engineer", "Go"))
        self.assertTrue(jobs.contains("C# and .NET developer", "C#"))

    def test_unknown_contract_is_review(self):
        result = jobs.assess(example(), {"contract_types": ["b2b"]})
        self.assertEqual(result["status"], "review")

    def test_remote_is_not_country_eligibility(self):
        self.assertEqual(jobs.assess(example(), {"country_filter": "EU"})["status"], "review")

    def test_closures_require_two_complete_scans(self):
        with tempfile.TemporaryDirectory() as directory, closing(jobs.connect(Path(directory) / "test.sqlite")) as db:
            self.assertEqual(jobs.ingest(db, "arbeitnow", [example()], True, "1")[0]["change"], "new")
            jobs.ingest(db, "arbeitnow", [], False, "2")
            self.assertEqual(db.execute("SELECT missing FROM jobs").fetchone()[0], 0)
            jobs.ingest(db, "arbeitnow", [], True, "3")
            self.assertEqual(db.execute("SELECT active FROM jobs").fetchone()[0], 1)
            jobs.ingest(db, "arbeitnow", [], True, "4")
            self.assertEqual(db.execute("SELECT active FROM jobs").fetchone()[0], 0)
            self.assertEqual(jobs.ingest(db, "arbeitnow", [example()], True, "5")[0]["change"], "reopened")

    def test_cursor_pagination(self):
        row = {"guid": "https://himalayas.app/jobs/test", "title": "Engineer", "companyName": "Example"}
        with patch("job_extractor.sources.fetch", side_effect=[{"jobs": [row], "nextCursor": "next"}, {"jobs": [], "nextCursor": None}]) as fetch:
            result, complete = collect("himalayas", {"max_pages": 3, "page_delay_seconds": 0, "retries": 0, "timeout_seconds": 2})
            self.assertTrue(complete)
            self.assertEqual(len(result), 1)
            self.assertIn("cursor=next", fetch.call_args.args[0])

    def test_remoteok_feed_skips_metadata_and_normalizes_tagged_jobs(self):
        feed = [
            {"last_updated": 123, "legal": "Attribution required"},
            {"id": "42", "slug": "remote-software-engineer-example-42",
             "position": "Remote Software Engineer", "company": "Example",
             "url": "https://remoteok.com/remote-jobs/remote-software-engineer-example-42",
             "location": "Europe", "description": "<p>Build products</p>",
             "date": "2026-10-08T10:00:00Z", "tags": ["dev", "python"],
             "salary_min": 100000, "salary_max": 120000},
        ]
        settings = {"retries": 0, "timeout_seconds": 2}
        with patch("job_extractor.sources.fetch", return_value=feed) as fetch:
            rows, complete = collect("remoteok", settings, source_config={
                "type": "remoteok", "tags": ["dev", "software"]
            })
        self.assertTrue(complete)
        self.assertEqual(len(rows), 1)
        self.assertEqual(rows[0]["source_id"], "42")
        self.assertEqual(rows[0]["url"], feed[1]["url"])
        self.assertIn("python", rows[0]["description"])
        self.assertEqual(rows[0]["work_mode"], "remote")
        self.assertEqual(rows[0]["compensation"]["min"], 100000)
        self.assertIn("tags=dev%2Csoftware", fetch.call_args.args[0])

    def test_page_cap_is_incomplete(self):
        with patch("job_extractor.sources.fetch", return_value={"jobs": [], "nextCursor": "more"}):
            _, complete = collect("himalayas", {"max_pages": 1, "page_delay_seconds": 0, "retries": 0, "timeout_seconds": 2})
            self.assertFalse(complete)

    def test_adzuna_source_uses_api_and_normalizes_results(self):
        payload = {"count": 1, "results": [{
            "id": "123", "title": "C# Engineer", "company": {"display_name": "Example"},
            "location": {"display_name": "Berlin, Germany"}, "redirect_url": "https://jobs.example/123",
            "description": "Build .NET services", "created": "2026-10-08T10:00:00Z",
        }]}
        settings = {"retries": 0, "timeout_seconds": 2, "page_delay_seconds": 0}
        source = {"type": "adzuna", "app_id_env": "TEST_ADZUNA_ID", "app_key_env": "TEST_ADZUNA_KEY",
                  "countries": ["DE"], "max_requests_per_run": 2, "max_pages_per_query": 1}
        with patch.dict("os.environ", {"TEST_ADZUNA_ID": "id", "TEST_ADZUNA_KEY": "secret"}), \
             patch("job_extractor.sources.fetch", return_value=payload) as fetch:
            rows, complete = collect_adzuna(source, settings, {"technologies": ["C#"]})
        self.assertTrue(complete)
        self.assertEqual(len(rows), 1)
        self.assertEqual(rows[0]["source"], "adzuna")
        self.assertIn("/jobs/de/search/1?", fetch.call_args.args[0])

    def test_missing_optional_api_credentials_are_reportable(self):
        with patch.dict("os.environ", {}, clear=True):
            self.assertEqual(missing_credentials({"type": "adzuna"}), ["ADZUNA_APP_ID", "ADZUNA_APP_KEY"])
            self.assertTrue(missing_credentials({"type": "jooble", "api_key_envs": {}}))

    def test_jsonld_scraper_requires_terms_flag_and_normalizes_public_posting(self):
        source = {"type": "jsonld", "urls": ["https://jobs.example/post"], "terms_allow_automated_access": True}
        settings = {"timeout_seconds": 2, "page_delay_seconds": 0, "max_pages": 5}
        posting = {"@type": "JobPosting", "identifier": "job-1", "title": "C# Engineer",
                   "hiringOrganization": {"name": "Example"}, "url": "https://jobs.example/post",
                   "jobLocation": {"address": {"addressLocality": "Berlin", "addressCountry": "Germany"}},
                   "datePosted": "2026-10-08"}
        with patch("job_extractor.scraping._allowed", return_value=True), \
             patch("job_extractor.scraping._read_url", return_value="<html></html>"), \
             patch("job_extractor.scraping._extract_page", return_value=[posting]):
            rows, complete = collect_jsonld(source, settings)
        self.assertTrue(complete)
        self.assertEqual(rows[0]["company"], "Example")
        self.assertEqual(rows[0]["location"], "Berlin, Germany")
        with self.assertRaises(PermissionError):
            collect_jsonld({**source, "terms_allow_automated_access": False}, settings)

    def test_google_search_discovers_structured_jobs_only_on_allowed_domains(self):
        source = {"api_key_env": "TEST_CSE_KEY", "engine_id_env": "TEST_CSE_ID",
                  "allowed_domains": ["jobs.example"], "terms_allow_automated_access": True,
                  "max_queries_per_run": 2, "max_results_per_query": 10}
        settings = {"timeout_seconds": 2, "page_delay_seconds": 0, "retries": 0}
        posting = {"@type": "JobPosting", "identifier": "job-2", "title": "C# Engineer",
                   "hiringOrganization": {"name": "Example"}, "url": "https://jobs.example/post"}
        with patch.dict("os.environ", {"TEST_CSE_KEY": "key", "TEST_CSE_ID": "engine"}), \
             patch("job_extractor.sources.fetch", return_value={"items": [
                 {"link": "https://jobs.example/post"}, {"link": "https://unapproved.example/post"}
             ]}) as fetch, \
             patch("job_extractor.scraping._allowed", return_value=True), \
             patch("job_extractor.scraping._extract_page", return_value=[posting]):
            rows, complete = collect_google_cse(source, settings, {"roles": ["Developer"], "technologies": ["C#"]})
        self.assertTrue(complete)
        self.assertEqual(len(rows), 1)
        self.assertEqual(rows[0]["source"], "google_cse")
        queries = [call.args[0] for call in fetch.call_args_list]
        self.assertTrue(any("Developer" in query for query in queries))
        self.assertTrue(any("C%23" in query for query in queries))

    def test_unchanged_and_updated_records(self):
        with tempfile.TemporaryDirectory() as directory, closing(jobs.connect(Path(directory) / "test.sqlite")) as db:
            job = example()
            jobs.ingest(db, "arbeitnow", [job], True, "1")
            self.assertEqual(jobs.ingest(db, "arbeitnow", [job], True, "2"), [])
            job["description"] = "Updated requirements"
            self.assertEqual(jobs.ingest(db, "arbeitnow", [job], True, "3")[0]["change"], "changed")
            self.assertEqual(db.execute("SELECT first_seen,last_seen FROM jobs").fetchone(), ("1", "3"))

    def test_export_escapes_markup(self):
        with tempfile.TemporaryDirectory() as directory, closing(jobs.connect(Path(directory) / "test.sqlite")) as db:
            base = Path(directory)
            job = example()
            job["title"] = "<script>alert(1)</script>"
            job["raw"]["title"] = job["title"]
            jobs.ingest(db, "arbeitnow", [job], True, "1")
            jobs.export(db, {"preferences": {}, "output_directory": "out"}, base, {"finished_at": "today", "events": []})
            content = (base / "out/jobs.html").read_text(encoding="utf-8")
            self.assertIn("&lt;script&gt;alert(1)&lt;/script&gt;", content)
            self.assertNotIn("<td><script>", content)
            self.assertEqual(len(json.loads((base / "out/jobs.json").read_text(encoding="utf-8"))), 1)


if __name__ == "__main__":
    unittest.main()
