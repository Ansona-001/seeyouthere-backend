-- Event music: at most one uploaded MP3 per event (event_audio_event_key). As with media, the file
-- is written before the row and deleted before the row; orphans are swept by storage reconcile
-- (LiveMediaIDs also reports event_audio ids so live audio is never swept).

-- name: GetEventAudio :one
-- Host view. The caller has already authorised the event (GetEventForUser).
SELECT id, event_id, uploaded_by, size_bytes, duration_ms, created_at
FROM event_audio
WHERE event_id = @event_id::uuid;

-- name: CreateEventAudio :one
-- Owner or editor, after LockEventForEditor, the quota check and DeleteEventAudioByEvent (replace)
-- in the same tx. A second row for the event violates event_audio_event_key.
INSERT INTO event_audio (id, event_id, uploaded_by, size_bytes, duration_ms)
SELECT @id::uuid, @event_id::uuid, @user_id::uuid, @size_bytes::bigint, @duration_ms::int
WHERE event_role(@event_id::uuid, @user_id::uuid) IN ('owner', 'editor')
RETURNING id, event_id, uploaded_by, size_bytes, duration_ms, created_at;

-- name: DeleteEventAudioByEvent :one
-- Replacement inside the upload tx; the caller has already authorised the event. Returns what the
-- caller needs to delete the old file. No row: the event had no audio.
DELETE FROM event_audio
WHERE event_id = @event_id::uuid
RETURNING id, size_bytes;

-- name: DeleteEventAudio :one
-- Owner or editor.
DELETE FROM event_audio a
WHERE a.event_id = @event_id::uuid
  AND a.id = @audio_id::uuid
  AND event_role(@event_id::uuid, @user_id::uuid) IN ('owner', 'editor')
RETURNING a.id;

-- name: EventAudioExists :one
-- Content save: whether the event has an uploaded track (music_source = 'custom' needs one).
SELECT EXISTS (SELECT 1 FROM event_audio WHERE event_id = @event_id::uuid);

-- name: GetServableAudio :one
-- Public audio serving gate, same visibility rule as GetServableMedia: the event is neither
-- soft-deleted nor taken down. No row = not servable, whatever the reason (the caller answers every
-- case with the same 404).
SELECT a.id
FROM event_audio a
JOIN events e ON e.id = a.event_id
WHERE a.id = @audio_id::uuid
  AND a.event_id = @event_id::uuid
  AND e.deleted_at IS NULL
  AND e.status <> 'taken_down';

-- name: ClearCustomMusic :exec
-- Deleting an event's track: a 'custom' choice falls back to 'none' (it must not silently revert to the
-- template's music). Deliberately no version bump: the editor's content version guards content, and the
-- host UI refetches the event after deleting audio. Run under LockEventForEditor like every audio write.
UPDATE events
SET music_source = 'none'
WHERE id = @event_id::uuid
  AND music_source = 'custom';
