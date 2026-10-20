-- The user's lift of claude's pacing (loop pace normal, #316): when it was set and until when
-- (unix ns; 0: not lifted / no time, that is until the next limit hit).
ALTER TABLE control ADD COLUMN pace_normal_set INTEGER NOT NULL DEFAULT 0;
ALTER TABLE control ADD COLUMN pace_normal_until INTEGER NOT NULL DEFAULT 0;
