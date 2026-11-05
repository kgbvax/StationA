@echo off
rem Allows inbound UDP for chaseq-ui.exe so Log4OM's reply (its inbound port + 1,
rem sent as an IPv4 broadcast) reaches the window. Without it the Log4OM part of
rem the result strip says "sent, no reply"; the callsign still arrives.
rem
rem Run once as administrator on bwpc (right-click, "Run as administrator").
rem Optional first argument: the full path of chaseq-ui.exe.
rem Remove the rule again with:  netsh advfirewall firewall delete rule name="chaseq-ui"

net session >nul 2>&1
if errorlevel 1 (
  echo This needs an administrator prompt: right-click the file and choose "Run as administrator".
  pause
  exit /b 1
)

set "EXE=%~1"
if "%EXE%"=="" set "EXE=C:\Users\iotte\chaseq-ui\chaseq-ui.exe"
if not exist "%EXE%" (
  echo chaseq-ui.exe not found at "%EXE%". Pass the right path as the first argument.
  pause
  exit /b 1
)

netsh advfirewall firewall delete rule name="chaseq-ui" >nul 2>&1
netsh advfirewall firewall add rule name="chaseq-ui" dir=in action=allow program="%EXE%" protocol=UDP profile=private,domain
if errorlevel 1 (
  echo The rule could not be added.
  pause
  exit /b 1
)
echo Rule "chaseq-ui" added for %EXE% (inbound UDP, private and domain networks).
echo If this PC's network is "public", run again after changing the profile in the script to private,domain,public.
pause
