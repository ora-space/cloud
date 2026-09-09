param([ValidateSet('start','stop')][string]$Action='start')
$ErrorActionPreference='Stop'
$workspace=Split-Path $PSScriptRoot -Parent
$localRoot=Join-Path $workspace '.local'
$pgRoot=Join-Path $localRoot 'postgres'
$pgBin=Join-Path $pgRoot 'pgsql/bin'
$pgData=Join-Path $localRoot 'pgdata'
if ($Action -eq 'stop') {
  & (Join-Path $pgBin 'pg_ctl.exe') -D $pgData stop -m fast
  if ($LASTEXITCODE -ne 0) {throw 'PostgreSQL stop failed'}
  exit
}
New-Item -ItemType Directory -Force $localRoot | Out-Null
if (-not (Test-Path (Join-Path $pgBin 'initdb.exe'))) {
  $archive=Join-Path $localRoot 'postgres.zip'
  if (-not (Test-Path $archive)) {Invoke-WebRequest 'https://sbp.enterprisedb.com/getfile.jsp?fileid=1260491' -OutFile $archive}
  $expected='4B8DB0930C38F6EF845DB919551DEDDA3B6B845AEB0927B3D79A6E8E9E4537CF'
  if ((Get-FileHash -LiteralPath $archive -Algorithm SHA256).Hash -ne $expected) {throw 'PostgreSQL 17.11 archive checksum mismatch'}
  Expand-Archive -LiteralPath $archive -DestinationPath $pgRoot
}
if (-not (Test-Path (Join-Path $pgData 'PG_VERSION'))) {
  & (Join-Path $pgBin 'initdb.exe') -D $pgData -U postgres -A trust --encoding=UTF8 --locale=C
  if ($LASTEXITCODE -ne 0) {throw 'initdb failed'}
}
& (Join-Path $pgBin 'pg_isready.exe') -h 127.0.0.1 -p 55432 | Out-Null
if ($LASTEXITCODE -ne 0) {
  $args=@('-D',('"'+$pgData+'"'),'-l',('"'+(Join-Path $localRoot 'pg.log')+'"'),'-o','"-h 127.0.0.1 -p 55432"','start','-w')
  $pgLauncher=Start-Process -FilePath (Join-Path $pgBin 'pg_ctl.exe') -ArgumentList $args -WindowStyle Hidden -PassThru
  if (-not $pgLauncher.WaitForExit(20000)) {throw 'pg_ctl start timed out; inspect .local/pg.log'}
  if ($pgLauncher.ExitCode -ne 0) {throw 'pg_ctl start failed; inspect .local/pg.log'}
}
$databaseExists=& (Join-Path $pgBin 'psql.exe') -h 127.0.0.1 -p 55432 -U postgres -d postgres -tAc "SELECT 1 FROM pg_database WHERE datname='ora_test'"
if ($databaseExists -ne '1') {
  & (Join-Path $pgBin 'createdb.exe') -h 127.0.0.1 -p 55432 -U postgres ora_test
  if ($LASTEXITCODE -ne 0) {throw 'createdb failed'}
}
Write-Output "TEST_DATABASE_URL=host=127.0.0.1 port=55432 user=postgres dbname=ora_test sslmode=disable"
