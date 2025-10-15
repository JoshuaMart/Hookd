# frozen_string_literal: true

require 'net/http'
require 'json'
require 'uri'

module Hookd
  # HTTP client for interacting with Hookd server
  class Client
    attr_reader :server, :token

    def initialize(server:, token:)
      @server = server
      @token = token
      @uri = URI.parse(server)
    end

    # Register a new hook
    # @return [Hookd::Hook] the newly registered hook
    # @raise [Hookd::AuthenticationError] if authentication fails
    # @raise [Hookd::ServerError] if server returns 5xx
    # @raise [Hookd::ConnectionError] if connection fails
    def register
      response = post('/register')
      Hook.from_hash(response)
    end

    # Poll for interactions on a hook
    # @param hook_id [String] the hook ID to poll
    # @return [Array<Hookd::Interaction>] array of interactions (may be empty)
    # @raise [Hookd::AuthenticationError] if authentication fails
    # @raise [Hookd::NotFoundError] if hook not found
    # @raise [Hookd::ServerError] if server returns 5xx
    # @raise [Hookd::ConnectionError] if connection fails
    def poll(hook_id)
      response = get("/poll/#{hook_id}")

      # Response is {"interactions": [...]}
      interactions = response['interactions']
      return [] if interactions.nil? || interactions.empty? || !interactions.is_a?(Array)

      interactions.map { |i| Interaction.from_hash(i) }
    rescue NoMethodError => e
      raise Error, "Invalid response format: #{e.message}"
    end

    # Get server metrics (requires authentication)
    # @return [Hash] metrics data
    # @raise [Hookd::AuthenticationError] if authentication fails
    # @raise [Hookd::ServerError] if server returns 5xx
    # @raise [Hookd::ConnectionError] if connection fails
    def metrics
      get('/metrics')
    end

    private

    def get(path)
      request = Net::HTTP::Get.new(path)
      request['Authorization'] = "Bearer #{token}"
      execute_request(request)
    end

    def post(path, body = nil)
      request = Net::HTTP::Post.new(path)
      request['Authorization'] = "Bearer #{token}"
      request['Content-Type'] = 'application/json'
      request.body = body.to_json if body
      execute_request(request)
    end

    def execute_request(request)
      http = Net::HTTP.new(@uri.host, @uri.port)
      http.use_ssl = @uri.scheme == 'https'
      http.open_timeout = 10
      http.read_timeout = 30

      response = http.request(request)

      case response.code.to_i
      when 200, 201
        raise Error, 'Empty response body from server' if response.body.nil? || response.body.empty?

        JSON.parse(response.body)
      when 401
        raise AuthenticationError, "Authentication failed: #{response.body}"
      when 404
        raise NotFoundError, "Resource not found: #{response.body}"
      when 500..599
        raise ServerError, "Server error (#{response.code}): #{response.body}"
      else
        raise Error, "Unexpected response (#{response.code}): #{response.body}"
      end
    rescue SocketError, Errno::ECONNREFUSED, Net::OpenTimeout, Net::ReadTimeout => e
      raise ConnectionError, "Connection failed: #{e.message}"
    rescue JSON::ParserError => e
      raise Error, "Invalid JSON response: #{e.message}"
    end
  end
end
