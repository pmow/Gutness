# Aural Organizer

Recognizes audiobooks by **listening** to them, then (as a last step) files
them into the folder layout Audiobookshelf expects.

Runs as a small Go app on Alpine, next to a Whisper speech-to-text container
and an optional LLM (Ollama), all from `docker-compose.yml`. Web UI on port **8078**.

## Pipeline, per library folder

1. **Scan.** Every folder holding audio is recorded in the database (SQLite in
   `/data`). Folder names are stored but not used to identify books. Each
   folder is classed as:
   - **set**: files numbered from 1 (`1`, `01` or `001`), every name also
     containing the total (e.g. `Part 07 of 53.mp3`, `07-53.mp3`), all parts
     1..N present, and no other audio files in the folder. A set is one book.
   - **single**: exactly one audio file.
   - **not a set**: anything else, with the reason shown (e.g. "names say 53
     parts but only 52 audio files are here; missing part 17"). These are
     listed but not sampled.
2. **Path rules.** An ordered list of regular expressions (Settings page) is
   tried against the path of each book's first file; the first that matches
   gives the author, title and series shown on the book. Named groups:
   `author`, `title` (required), `series`, `index`, `narrator`, `year`.
   Defaults cover `Author/Series/01 - Title/…`, `Author - Title (2021)/…` and
   `Author/Title/…`. Options: match the full path instead of the path inside
   the library; accept a path match without listening; and (on by default)
   send a listening result that names a different book than the path to
   review instead of matching it.
3. **Recognize** with the enabled methods, in order. When one can't identify
   the book, the next takes over (`methods.go`; each method is a small Go type,
   so more can be added).

   **Method 1, Title announcement.** Transcribes the first 15 seconds of part 1
   and looks for `[Someone presents:] The Title, by Author[, read by Narrator]`.
   Title and author are required; the rest is optional. Pattern matching reads
   it; the LLM is asked only if the pattern finds nothing (optional). The title
   and author are then searched in Audible's catalog, Open Library and Google
   Books, and the results scored against what was heard: 80%+ is matched,
   45%+ goes to review.

   **Method 2, Text search.** Transcribes a random 30 seconds from the middle
   80% of the whole set and searches the text inside books (Google Books, and
   Open Library's "search inside" over Internet Archive scans). One book found
   is a match. Two or more: take another random 30 seconds and search with all
   60 seconds of words. Still several: you pick from the list.
4. **Lookups are queued.** A background worker per catalog sends searches only
   as fast as that catalog's limits allow and pauses on HTTP 429/503
   (honouring `Retry-After`, otherwise backing off 1, 5, 15, 60 minutes).
   Call history is stored, so a restart keeps the budget.
5. **Organize (final step, manual).** Matched books move to
   `Author/Series/Vol N - Title` or `Author/Title` inside their library.
   Preview first, never overwrites, held on any conflict, undo from a journal.

## Web UI

Four tabs, with a status bar on every page: Whisper and LLM status, and for
each catalog its state, a live countdown until it may be called again, usage
against each limit, and its queue size.

- **Queue**: sets still being identified, with a chip per stage (sample,
  transcript, reading, catalog search, needs review, no match, error). Each
  row shows the parent and containing folder, the number of audio files with
  an expandable file list, what the path rules say, and a *Move to top*
  button that makes the set the next one processed. *Scan library folders*
  at the top searches every folder listed in Settings for sets. A link leads
  to the catalog search queue.
- **Completed**: identified sets with author, title, series, a link (Audible
  by ASIN, else Open Library by ISBN, else the catalog page), the method(s)
  used, and Yes/No for whether the result matches the path rule's author and
  title. Every column sorts (click the heading) and filters (boxes under the
  headings). Links to Organize, the final move, with undo.
- **All**: the same table for every set, with a Status column; sets not yet
  identified have blanks.
- **Settings**: library folders (add, remove; removing only forgets them in
  the app), both methods (on/off, sample start and length, middle
  percentage, extra samples, words per search, catalogs, score thresholds,
  LLM fallback, whether a single text-search hit counts as a match), path
  rules with a tester, catalog limits and the Whisper language. Saved in the
  database and applied immediately.

Clicking a set opens its page: every sample with a player and its transcript,
catalog results with "Use this", manual entry, and buttons to start over or
run text search.

## Run

    AUDIOBOOKS=/path/to/audiobooks docker compose up -d
    # open http://<host>:8078

The library is mounted at `/audiobooks` and added automatically; add more
folders (subfolders, or other mounts) on the Settings tab. The first start downloads
the Whisper and LLM models.

## Environment

| Variable | Default | |
|---|---|---|
| `LIBRARIES` | | `:`-separated folders to add on start |
| `WHISPER_URL` | `http://localhost:9000` | |
| `OLLAMA_URL`, `OLLAMA_MODEL` | off, `llama3.2:3b` | the model is pulled automatically |
| `LOOKUP_APIS` | `audible,openlibrary,googlebooks` | |
| `AUDIBLE_REGION` | `com` | `co.uk`, `de`, `ca`, `com.au`, … |
| `GOOGLE_BOOKS_API_KEY` | | optional, raises Google's daily limit |
| `RESCAN_HOURS` | 6 | |
| `API_LIMITS`, `WHISPER_LANGUAGE` | | first-run defaults; then use the Settings page |

## Models

Both download themselves on first start: Whisper's `base.en` model (from
Hugging Face, via the whisper container) and the LLM (`llama3.2:3b`, about
2 GB, pulled by the app through Ollama). To fetch the LLM by hand:
`docker compose exec ollama ollama pull llama3.2:3b`. The LLM is optional;
remove `OLLAMA_URL` to run without it.

The UI has no login; keep port 8078 on your home network.
