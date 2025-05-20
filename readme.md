# Traefik Queue Manager Middleware Plugin

A Traefik middleware plugin that implements a queue management system for your services, helping to manage traffic spikes by limiting the number of concurrent users and providing a fair waiting experience.

## How It Works

When traffic exceeds your configured capacity:
1. New visitors are placed in a queue
2. Users are shown their position in the queue with estimated wait time
3. The queue page automatically refreshes at configurable intervals
4. When capacity becomes available, visitors are let in based on first-come, first-served

The plugin uses a client identifier (cookie or IP+UserAgent hash) to track visitors and ensure a fair queuing system.

## Features

- Configurable maximum number of concurrent users
- Custom queue page template
- Adjustable expiration time for sessions
- Option to use cookies or IP+UserAgent hash for visitor tracking
- Real-time capacity monitoring
- Visual progress indication for waiting users

## Installation

### From Traefik Pilot (Recommended)

```yaml
# Static configuration
experimental:
  plugins:
    queuemanager:
      moduleName: "github.com/hhftechnology/traefik-queue-manager"
      version: "v1.0.0"
```

### Manual Installation

1. Clone the repository:
   ```
   git clone https://github.com/hhftechnology/traefik-queue-manager.git
   ```

2. Build and install the plugin:
   ```
   cd traefik-queue-manager
   go build -o traefik-queue-manager
   ```

3. Configure Traefik to use the local plugin:
   ```yaml
   # Static configuration
   experimental:
     localPlugins:
       queuemanager:
         moduleName: "github.com/hhftechnology/traefik-queue-manager"
   ```

## Configuration

### Plugin Configuration Options

| Option | Type | Default | Description |
|--------|------|---------|-------------|
| `enabled` | boolean | `true` | Enable/disable the queue manager |
| `queuePageFile` | string | `queue-page.html` | Path to the queue page HTML template |
| `sessionTime` | duration | `1m` | Duration for which a visitor session is valid |
| `purgeTime` | duration | `5m` | How often expired sessions are purged from cache |
| `maxEntries` | int | `100` | Maximum number of concurrent users allowed |
| `httpResponseCode` | int | `429` | HTTP response code for queue page |
| `httpContentType` | string | `text/html; charset=utf-8` | Content type of queue page |
| `useCookies` | boolean | `true` | Use cookies for tracking; if false, uses IP+UserAgent hash |
| `cookieName` | string | `queue-manager-id` | Name of the cookie used for tracking |
| `cookieMaxAge` | int | `3600` | Max age of the cookie in seconds |
| `refreshInterval` | int | `30` | Refresh interval in seconds |
| `debug` | boolean | `false` | Enable debug logging |

### Example Configuration

```yaml
# Dynamic configuration with Docker provider
labels:
  - traefik.http.middlewares.queuemanager.plugin.queuemanager.enabled=true
  - traefik.http.middlewares.queuemanager.plugin.queuemanager.queuePageFile=/path/to/queue-page.html
  - traefik.http.middlewares.queuemanager.plugin.queuemanager.maxEntries=500
  - traefik.http.middlewares.queuemanager.plugin.queuemanager.sessionTime=5m
  - traefik.http.middlewares.queuemanager.plugin.queuemanager.useCookies=true
  - traefik.http.middlewares.queuemanager.plugin.queuemanager.cookieName=queue-manager-id
```

## Customizing the Queue Page

You can customize the queue page by modifying the HTML template. The template supports the following variables:

- `[[.Position]]` - Current position in queue
- `[[.QueueSize]]` - Total queue size 
- `[[.EstimatedWaitTime]]` - Estimated wait time in minutes
- `[[.RefreshInterval]]` - Refresh interval in seconds
- `[[.ProgressPercentage]]` - Visual progress percentage
- `[[.Message]]` - Custom message

Example template:

```html
<!DOCTYPE html>
<html>
<head>
    <title>Service Queue</title>
    <meta http-equiv="refresh" content="[[.RefreshInterval]]">
    <style>
        body {
            font-family: Arial, sans-serif;
            text-align: center;
            margin-top: 50px;
        }
        .container {
            max-width: 600px;
            margin: 0 auto;
            padding: 20px;
            border: 1px solid #ddd;
            border-radius: 5px;
        }
        .progress {
            margin: 20px 0;
            height: 20px;
            background-color: #f5f5f5;
            border-radius: 4px;
            overflow: hidden;
        }
        .progress-bar {
            height: 100%;
            background-color: #4CAF50;
            text-align: center;
            line-height: 20px;
            color: white;
        }
    </style>
</head>
<body>
    <div class="container">
        <h1>You're in the queue</h1>
        <p>Our service is currently at capacity. Please wait and you'll be automatically redirected when space becomes available.</p>
        
        <div class="progress">
            <div class="progress-bar" style="width: [[.ProgressPercentage]]%">
                [[.ProgressPercentage]]%
            </div>
        </div>
        
        <p>Your position in queue: [[.Position]] of [[.QueueSize]]</p>
        <p>Estimated wait time: [[.EstimatedWaitTime]] minutes</p>
        <p>This page will refresh automatically in [[.RefreshInterval]] seconds.</p>
    </div>
</body>
</html>
```

## Example Usage with Docker Compose

```yaml
services:
  traefik:
    image: traefik:v3.3.4
    container_name: traefik
    command:
      - --log.level=INFO
      - --api.insecure=true
      - --providers.docker=true
      - --entrypoints.web.address=:80
      - --experimental.localPlugins.queuemanager.moduleName=github.com/hhftechnology/traefik-queue-manager
    ports:
      - "80:80"
      - "8080:8080"
    volumes:
      - /var/run/docker.sock:/var/run/docker.sock
      - ./:/plugins-local/src/github.com/hhftechnology/traefik-queue-manager
    labels:
      - traefik.http.middlewares.queuemanager.plugin.queuemanager.enabled=true
      - traefik.http.middlewares.queuemanager.plugin.queuemanager.queuePageFile=/plugins-local/src/github.com/hhftechnology/traefik-queue-manager/queue-page.html
      - traefik.http.middlewares.queuemanager.plugin.queuemanager.maxEntries=5
      - traefik.http.middlewares.queuemanager.plugin.queuemanager.sessionTime=1m

  myservice:
    image: traefik/whoami
    container_name: myservice
    labels:
      - traefik.enable=true
      - traefik.http.routers.myservice.rule=Host(`service.local`)
      - traefik.http.routers.myservice.entrypoints=web
      - traefik.http.routers.myservice.middlewares=queuemanager
```

## Performance Considerations

The plugin uses an in-memory cache to track active sessions, which provides excellent performance but means:

1. If you're running multiple Traefik instances, each will maintain its own separate queue
2. If Traefik restarts, all queue data is lost

For high-availability setups, consider using a shared Redis cache or similar solution.

## Contributing

Contributions are welcome! Please feel free to submit a Pull Request.

## License

This project is licensed under the MIT License - see the LICENSE file for details.