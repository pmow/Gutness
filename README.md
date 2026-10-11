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
2. **Sample.** ffmpeg cuts 30 seconds (`SAMPLE_SECONDS`, from `SAMPLE_START`)
   from part 1 as 16 kHz mono WAV. You can play it on the book's page.
3. **Transcribe** with Whisper.
4. **Read the transcript.** The LLM extracts title, author, narrator and
   series from what was said ("This is Audible. The Way of Kings, by…").
   Without an LLM, pattern matching does the same for the common phrasings.
5. **Look up** in Audible's catalog, Open Library and Google Books. Each
   search is a job in a queue; a background worker per catalog sends them
   only as fast as that catalog's limits allow and pauses on HTTP 429/503
   (honouring `Retry-After`, otherwise backing off 1, 5, 15, 60 minutes).
   Call history is stored, so a restart keeps the budget.
6. **Score.** Results are compared with what was heard. 80%+ is **matched**,
   45%+ is **needs review** (pick a result or type the details yourself),
   lower is **no match**.
7. **Organize (final step, manual).** Matched books move to
   `Author/Series/Vol N - Title` or `Author/Title` inside their library.
   Preview first, never overwrites, held on any conflict, undo from a journal.

## Web UI

- Top bar on every page: Whisper and LLM status, and for each catalog its
  state, a live countdown until it may be called again, usage against each
  limit, and its queue size.
- **Books**: filter tabs with counts (defaults to *Pending recognition*),
  library picker and search across folder, title, author and transcript.
- **Lookup queue**: every catalog search, filterable by state and catalog.
- **Organize**: the final move, with undo.

## Run

    AUDIOBOOKS=/path/to/audiobooks docker compose up -d
    # open http://<host>:8078

The library is mounted at `/audiobooks` and added automatically; add more
folders (subfolders, or other mounts) from the UI. The first start downloads
the Whisper and LLM models.

## Settings (environment)

| Variable | Default | |
|---|---|---|
| `LIBRARIES` | | `:`-separated folders to add on start |
| `SAMPLE_SECONDS` / `SAMPLE_START` | 30 / 0 | sample length and offset |
| `WHISPER_URL`, `WHISPER_LANGUAGE` | `http://localhost:9000`, `en` | |
| `OLLAMA_URL`, `OLLAMA_MODEL` | off, `llama3.2:3b` | the model is pulled automatically |
| `LOOKUP_APIS` | `audible,openlibrary,googlebooks` | which catalogs to search |
| `API_LIMITS` | see below | e.g. `audible=1/2s,300/1h;googlebooks=1/1s,1000/24h` |
| `AUDIBLE_REGION` | `com` | `co.uk`, `de`, `ca`, `com.au`, … |
| `GOOGLE_BOOKS_API_KEY` | | optional |
| `RESCAN_HOURS` | 6 | |

Default limits: Audible 1 per 2 s and 300 per hour; Open Library 1 per s and
500 per hour; Google Books 1 per s and 1000 per day.

The UI has no login; keep port 8078 on your home network.
