-- +goose Up

-- Wisteria reads as botanical stationery: the painted canopy is the only ornament and the content sits
-- straight on the paper, with no rules or boxes anywhere.
--
-- Colours follow the artwork (white blossom on green leaves, gold): the names (hero_ink, art4) move from
-- a violet that is not in the painting to a deep leaf green, text and muted to green-greys, the accent
-- (buttons) to leaf green, and the foil to a deeper gold so the script ampersand reads on ivory (the old
-- foil ended in near-white #F7E8B5). Every text colour clears 4.5:1 on its background and surface.
--
-- Borders go: the page frame layer is dropped, cards become `open` (no box) with no border, the date
-- loses its bars, the divider becomes a leafy sprig instead of a double rule, and the foil seal (which sat
-- on a card edge) is removed.
--
-- Published template versions are immutable, so Wisteria's latest published version is copied into a new
-- version with only `palettes`, `layers`, `ornament` and `card` changed; palette and font-pair ids are
-- unchanged, so an event's choices carry over. Events pinned to the old version are offered the update in
-- the editor. A no-op when there is no published Wisteria version.
INSERT INTO template_versions (template_id, version, manifest, assets_path, created_by, published_at)
SELECT v.template_id,
       (SELECT max(v2.version) FROM template_versions v2 WHERE v2.template_id = v.template_id) + 1,
       v.manifest
         || jsonb_build_object(
              'layers', (SELECT jsonb_agg(e.layer ORDER BY e.ord)
                         FROM jsonb_array_elements(v.manifest->'layers') WITH ORDINALITY AS e(layer, ord)
                         WHERE e.layer->>'kind' <> 'frame'),
              'ornament', (v.manifest->'ornament') - 'date' || '{"divider":"olive_sprig","badge":"none"}'::jsonb,
              'card', (v.manifest->'card') || '{"style":"open","border":"none"}'::jsonb)
         || jsonb_build_object('palettes', $palettes$[
         {"id":"ivory","name":"Ivory",
          "colors":{"background":"#F6F1E6","surface":"#FCF9F2","text":"#2E3A2B","muted":"#5B6852","accent":"#4D6140","accent_text":"#FFFFFF"},
          "accent_ink":"#4D6140","art":["#7D6224","#FFFDF8","#E2DCC6","#3F5236"],"foil":["#7E6020","#B8923C","#E6CF8E"]},
         {"id":"sage","name":"Sage",
          "colors":{"background":"#EEF1E8","surface":"#F8FAF4","text":"#2F382F","muted":"#56604F","accent":"#4A5E3A","accent_text":"#FFFFFF"},
          "accent_ink":"#4A5E3A","art":["#76601F","#FBFCF7","#D5DCC8","#3B4C33"],"foil":["#7E6020","#B8923C","#E6CF8E"]}
       ]$palettes$::jsonb),
       v.assets_path,
       NULL,
       now()
FROM template_versions v
JOIN templates t ON t.id = v.template_id
WHERE t.slug = 'wisteria'
  AND v.published_at IS NOT NULL
  AND v.version = (SELECT max(v3.version) FROM template_versions v3
                   WHERE v3.template_id = v.template_id AND v3.published_at IS NOT NULL);

-- +goose Down
-- Dev only, same pattern as 00027 (published versions are immutable, so the trigger is disabled for the
-- delete). Removes Wisteria's latest version when it is the one this migration added (leaf-green names
-- in art4). Fails while any event is still pinned to it (events -> template_versions foreign key).
ALTER TABLE template_versions DISABLE TRIGGER template_versions_immutable;
DELETE FROM template_versions nv
USING templates t
WHERE t.slug = 'wisteria'
  AND nv.template_id = t.id
  AND nv.version = (SELECT max(v2.version) FROM template_versions v2 WHERE v2.template_id = t.id)
  AND nv.manifest #>> '{palettes,0,art,3}' = '#3F5236';
ALTER TABLE template_versions ENABLE TRIGGER template_versions_immutable;
