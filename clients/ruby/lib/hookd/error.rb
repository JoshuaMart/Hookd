# frozen_string_literal: true

module Hookd
  # Base error class for all Hookd errors
  class Error < StandardError; end

  # Raised when authentication fails (401)
  class AuthenticationError < Error; end

  # Raised when a resource is not found (404)
  class NotFoundError < Error; end

  # Raised when there's a connection error
  class ConnectionError < Error; end

  # Raised when the server returns a 5xx error
  class ServerError < Error; end
end
