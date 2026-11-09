FROM ruby:3.3.13-slim
ARG SIDEKIQ_VERSION
RUN apt-get update \
 && apt-get install -y --no-install-recommends redis-server procps build-essential \
 && gem install --no-document sidekiq -v "$SIDEKIQ_VERSION" \
 && apt-get purge -y --auto-remove build-essential \
 && rm -rf /var/lib/apt/lists/*
COPY app /srv/myapp
COPY check-sidekiq-status /usr/local/bin/check-sidekiq-status
CMD ["redis-server", "--save", "", "--appendonly", "no"]
