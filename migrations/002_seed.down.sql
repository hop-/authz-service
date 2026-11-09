-- Removes only the rows 002_seed.up.sql inserted, by natural key. Roles,
-- permissions and pairs created later through the admin API stay.
--
-- WARNING: foreign keys cascade. Deleting a seeded role also deletes its
-- user_roles assignments, and deleting a seeded permission also deletes the
-- role_permissions and user_permission_overrides that reference it, including
-- ones created later for non-seeded roles.

DELETE FROM role_permissions rp
USING roles r, permissions p
WHERE rp.role_id = r.id
  AND rp.permission_id = p.id
  AND (r.name, p.action, p.resource_type) IN (
    ('viewer', 'read',   'document'),
    ('editor', 'read',   'document'),
    ('editor', 'write',  'document'),
    ('admin',  'read',   'document'),
    ('admin',  'write',  'document'),
    ('admin',  'delete', 'document')
  );

DELETE FROM permissions
WHERE (action, resource_type) IN (
    ('read',   'document'),
    ('write',  'document'),
    ('delete', 'document')
);

DELETE FROM roles
WHERE name IN ('viewer', 'editor', 'admin');
