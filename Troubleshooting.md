# Troubleshooting

## Notifications Not Received

```shell
# Check environment variables
systemctl --user show-environment | grep TELEGRAM

# Test Telegram API directly
curl -X POST "https://api.telegram.org/bot<TOKEN>/sendMessage" \
     -d "chat_id=<CHAT_ID>" -d "text=Test"

# Check service logs
journalctl --user -u telegram-notify@yourservice.service
```

---
<br>

## Permission Errors

```shell
# Verify binary permissions
ls -l ~/.local/bin/telegram-notifier

# For system services, verify SELinux context
ls -Z /usr/local/bin/telegram-notifier
```

---
<br>

## Service Not Triggering Notifications

```shell
# Verify OnFailure configuration
systemctl --user cat your-service.service | grep OnFailure

# Check if notification service exists
systemctl --user list-unit-files | grep telegram-notify
```
