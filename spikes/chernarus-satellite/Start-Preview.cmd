@echo off
setlocal
title Chernarus Satellite Preview
echo Chernarus satellite preview: http://127.0.0.1:8766/
echo Keep this window open while using the map. Press Ctrl+C to stop.
echo.
python -u "%~dp0serve.py" --data "%~dp0..\..\.scratch\chernarus-satellite" --port 8766
if errorlevel 1 (
    echo.
    echo The preview server could not start. See the error above.
    pause
)
