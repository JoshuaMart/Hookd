# frozen_string_literal: true

module Hookd
  # Represents a registered hook with its endpoints. smtp is set only when the
  # server runs a mail listener. expires_at and metadata are populated for
  # long-lived hooks (registered with a ttl) and nil otherwise. secret is the
  # owner secret a registered hook belongs to, on servers with owner secrets.
  class Hook
    attr_reader :id, :dns, :http, :https, :smtp, :created_at, :expires_at, :metadata, :secret

    def initialize(id:, dns:, http:, https:, created_at:, smtp: nil, expires_at: nil, metadata: nil, secret: nil)
      @id = id
      @dns = dns
      @http = http
      @https = https
      @smtp = smtp
      @created_at = created_at
      @expires_at = expires_at
      @metadata = metadata
      @secret = secret
    end

    # Create a Hook from API response hash
    def self.from_hash(hash, secret: nil)
      raise ArgumentError, "Invalid hash: expected Hash, got #{hash.class}" unless hash.is_a?(Hash)

      new(
        id: hash['id'],
        dns: hash['dns'],
        http: hash['http'],
        https: hash['https'],
        smtp: hash['smtp'],
        created_at: hash['created_at'],
        expires_at: hash['expires_at'],
        metadata: hash['metadata'],
        secret: secret
      )
    end

    def to_s
      "#<Hookd::Hook id=#{id} dns=#{dns}>"
    end
  end
end
