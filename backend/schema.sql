CREATE TABLE IF NOT EXISTS users (
    email TEXT PRIMARY KEY,
    primary_email BOOLEAN,
    verified BOOLEAN
);

CREATE TABLE IF NOT EXISTS repos (
    rehash TEXT PRIMARY KEY,
    initial_commit_rehash TEXT,
    repo_url TEXT,
    repo_name TEXT,
    last_commit_rehash TEXT,
    last_synced_at BIGINT,
    github_pushed_at TEXT
);

CREATE INDEX IF NOT EXISTS idx_repos_repo_url ON repos(repo_url);
CREATE INDEX IF NOT EXISTS idx_repos_repo_name ON repos(repo_name);
CREATE INDEX IF NOT EXISTS idx_repos_last_commit ON repos(last_commit_rehash);

CREATE TABLE IF NOT EXISTS commits (
    rehash TEXT PRIMARY KEY,
    repo_rehash TEXT REFERENCES repos(rehash),
    author_name TEXT,
    author_email TEXT,
    date BIGINT,
    is_qommit BOOLEAN,
    num_lines_added INTEGER,
    num_lines_deleted INTEGER
);

CREATE INDEX IF NOT EXISTS idx_commits_repo ON commits(repo_rehash);
CREATE INDEX IF NOT EXISTS idx_commits_date ON commits(date);
CREATE INDEX IF NOT EXISTS idx_commits_author_email ON commits(author_email);
CREATE INDEX IF NOT EXISTS idx_commits_repo_date ON commits(repo_rehash, date);
CREATE INDEX IF NOT EXISTS idx_commits_repo_author ON commits(repo_rehash, author_email);

CREATE TABLE IF NOT EXISTS commit_stats (
    id SERIAL PRIMARY KEY,
    commit_rehash TEXT REFERENCES commits(rehash),
    num_lines_added INTEGER,
    num_lines_deleted INTEGER,
    type INTEGER,
    tech TEXT,
    UNIQUE (commit_rehash, tech, type)
);

CREATE INDEX IF NOT EXISTS idx_commit_stats_commit ON commit_stats(commit_rehash);
CREATE INDEX IF NOT EXISTS idx_commit_stats_tech ON commit_stats(tech);

CREATE TABLE IF NOT EXISTS facts (
    id SERIAL PRIMARY KEY,
    repo_rehash TEXT REFERENCES repos(rehash),
    email TEXT,
    code INTEGER,
    key INTEGER,
    value1 TEXT,
    value2 TEXT,
    value3 TEXT,
    value4 TEXT,
    UNIQUE (repo_rehash, email, code, key)
);

CREATE INDEX IF NOT EXISTS idx_facts_repo_email ON facts(repo_rehash, email);
CREATE INDEX IF NOT EXISTS idx_facts_email ON facts(email);
CREATE INDEX IF NOT EXISTS idx_facts_email_code ON facts(email, code);

CREATE TABLE IF NOT EXISTS authors (
    email TEXT PRIMARY KEY,
    name TEXT,
    repo_rehash TEXT REFERENCES repos(rehash)
);

CREATE TABLE IF NOT EXISTS technologies (
    id TEXT PRIMARY KEY,
    name TEXT NOT NULL,
    lang TEXT NOT NULL,
    category TEXT NOT NULL,
    icon TEXT,
    description TEXT,
    import_tokens TEXT[]
);

CREATE INDEX IF NOT EXISTS idx_technologies_lang ON technologies(lang);
CREATE INDEX IF NOT EXISTS idx_technologies_category ON technologies(category);
