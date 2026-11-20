CREATE TABLE hosted_analytics_daily (
    day TEXT NOT NULL,
    metric TEXT NOT NULL CHECK (metric IN ('app_loads', 'privacy_loads', 'terms_loads', 'signups')),
    total INTEGER NOT NULL,
    PRIMARY KEY (day, metric)
);
