# Hookd Ruby Client

Ruby client library for [Hookd](https://github.com/JoshuaMart/hookd/server), a DNS/HTTP interaction server for security testing and debugging.

## Installation

Add this line to your application's Gemfile:

```ruby
gem 'hookd-client'
```

Or install it yourself as:

```bash
gem install hookd-client
```

## Usage

### Basic Example

```ruby
require 'hookd'
require 'typhoeus'

# Initialize the client
client = Hookd::Client.new(
  server: "https://hookd.example.com",
  token: ENV['HOOKD_TOKEN']
)

# Register a new hook
hook = client.register
puts "DNS endpoint: #{hook.dns}"
puts "HTTP endpoint: #{hook.http}"
puts "HTTPS endpoint: #{hook.https}"

# Make a request to the HTTP endpoint to simulate an interaction
Typhoeus.get(hook.http)

# Poll for interactions
interactions = client.poll(hook.id)
interactions.each do |interaction|
  if interaction.dns?
    puts "DNS query: #{interaction.data}"
  elsif interaction.http?
    puts "HTTP request: #{interaction.data}"
  end
end
```

### Configuration

The client requires two configuration parameters:

- `server`: The Hookd server URL (e.g., `https://hookd.example.com`)
- `token`: Authentication token for API access

### API Reference

#### `Hookd::Client`

Main client class for interacting with the Hookd server.

##### `#register(count: nil)`

Register one or more hooks and get DNS/HTTP endpoints.

**Single hook (default):**
```ruby
hook = client.register
# => #<Hookd::Hook id="abc123" dns="abc123.hookd.example.com" ...>
```

**Multiple hooks:**
```ruby
hooks = client.register(count: 5)
# => [#<Hookd::Hook id="abc123" ...>, #<Hookd::Hook id="def456" ...>, ...]
```

Parameters:
- `count` (Integer, optional) - Number of hooks to create (default: 1)

Returns:
- `Hookd::Hook` object when `count` is 1 or not specified
- Array of `Hookd::Hook` objects when `count` > 1

Raises:
- `ArgumentError` - Invalid count parameter
- `Hookd::AuthenticationError` - Authentication failed
- `Hookd::ServerError` - Server error (5xx)
- `Hookd::ConnectionError` - Connection failed

##### `#poll(hook_id)`

Poll for interactions captured by a hook.

```ruby
interactions = client.poll("abc123")
# => [#<Hookd::Interaction type="dns" ...>, ...]
```

Parameters:
- `hook_id` (String) - The hook ID to poll

Returns: Array of `Hookd::Interaction` objects

Raises:
- `Hookd::AuthenticationError` - Authentication failed
- `Hookd::NotFoundError` - Hook not found
- `Hookd::ServerError` - Server error (5xx)
- `Hookd::ConnectionError` - Connection failed

##### `#metrics`

Get server metrics (requires authentication).

```ruby
metrics = client.metrics
# => {"total_hooks" => 42, "total_interactions" => 1337, ...}
```

Returns: Hash with metrics data

#### `Hookd::Hook`

Represents a registered hook with endpoints.

Attributes:
- `id` (String) - Unique hook identifier
- `dns` (String) - DNS endpoint
- `http` (String) - HTTP endpoint
- `https` (String) - HTTPS endpoint
- `created_at` (String) - Creation timestamp

#### `Hookd::Interaction`

Represents a captured DNS or HTTP interaction.

Attributes:
- `type` (String) - Interaction type ("dns" or "http")
- `timestamp` (String) - When the interaction was captured
- `data` (Hash) - Interaction details

Methods:
- `#dns?` - Returns true if this is a DNS interaction
- `#http?` - Returns true if this is an HTTP interaction

### Error Handling

The client raises specific exceptions for different error conditions:

```ruby
begin
  hook = client.register
rescue Hookd::AuthenticationError
  puts "Invalid token"
rescue Hookd::ConnectionError => e
  puts "Connection failed: #{e.message}"
rescue Hookd::ServerError => e
  puts "Server error: #{e.message}"
end
```

Exception hierarchy:
- `Hookd::Error` (base class)
  - `Hookd::AuthenticationError` - 401 Unauthorized
  - `Hookd::NotFoundError` - 404 Not Found
  - `Hookd::ServerError` - 5xx Server Error
  - `Hookd::ConnectionError` - Network/connection errors
