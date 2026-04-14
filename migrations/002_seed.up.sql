INSERT INTO roles (name, description) VALUES
    ('viewer', 'Read-only access'),
    ('editor', 'Read and write access'),
    ('admin',  'Full access including delete and management')
ON CONFLICT DO NOTHING;

INSERT INTO permissions (action, resource_type, description) VALUES
    ('read',   'document', 'Read a document'),
    ('write',  'document', 'Write a document'),
    ('delete', 'document', 'Delete a document')
ON CONFLICT DO NOTHING;

INSERT INTO role_permissions (role_id, permission_id)
SELECT r.id, p.id FROM roles r, permissions p
WHERE (r.name, p.action, p.resource_type) IN (
    ('viewer', 'read',   'document'),
    ('editor', 'read',   'document'),
    ('editor', 'write',  'document'),
    ('admin',  'read',   'document'),
    ('admin',  'write',  'document'),
    ('admin',  'delete', 'document')
)
ON CONFLICT DO NOTHING;
