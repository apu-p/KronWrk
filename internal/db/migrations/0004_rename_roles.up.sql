-- Project renamed SuperCron -> Kronwrk. Rename the group roles created by
-- 0003; grants and table ownership follow automatically (role references are
-- OID-based, so privileges are preserved).
ALTER ROLE supercron_admin RENAME TO kronwrk_admin;
ALTER ROLE supercron_user RENAME TO kronwrk_user;
ALTER ROLE supercron_support RENAME TO kronwrk_support;
