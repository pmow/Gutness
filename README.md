# Aural Organizer

A small, self-contained web app that sorts an audiobook library into

    Author/Series/NN - Title/    (books in a series)
    Author/Title/                (standalone books)

It was written to cover the places Library Manager fell short: counts that
don't add up, and a hard dependency on an online API.

## How it works

1. **Scan (read-only).** Every audio file under the library folder is found
   and assigned to exactly one book. A count check on the page shows
   `ready + already in place + held = total` so nothing goes missing silently.
   - One folder = one book. `CD1`, `Disc 2`, `Part 3` subfolders join their parent.
   - A folder of several `.m4b` files with different album tags = one book each.
   - Loose audio files at the top of the library are one book each.
2. **Metadata**, in this order: embedded tags (album, album artist/artist,
   series/`MVNM`/grouping, series part/`MVIN`, narrator/composer, year), then
   folder and file names (`Author/Series/03 - Title`, `Author - Title (2021)`,
   `Title, Book 2`). Online lookup (Open Library) is a per-book button and
   only fills blank fields; if it times out nothing changes.
3. **Preview.** Each book shows where it would go. Books with no author or
   title, a file already at the target, or two books aiming at the same
   folder are **held** and never touched. You can fix any field on the page
   or tick "leave alone".
4. **Apply.** Moves (or copies, with the copy option) only the ready books.
   It never overwrites a file, falls back to copy-verify-delete across
   disks, removes source folders only once they are empty, and writes a
   journal so the **Undo** button can put everything back.

## Run it

    go build -o aural .
    ./aural -source "/path/to/audiobooks"            # then open http://127.0.0.1:8080
    ./aural -source /in -dest /out -copy             # copy into a new tree
    ./aural -plan -source "/path/to/audiobooks"      # print the plan, change nothing

The UI listens on 127.0.0.1 only.

## Supported formats

mp3, m4a, m4b, aac, flac, ogg/opus, wma, wav. Other files in a book's
folder (cover art, cue sheets, PDFs) move with the book.
